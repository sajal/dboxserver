package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nytimes/gziphandler"
	"github.com/stacktic/dropbox"
	"golang.org/x/crypto/acme/autocert"
)

var (
	db           *dropbox.Dropbox
	lmod         = time.Now()
	errNotCached = fmt.Errorf("Object not found in cache")
	dbcache      = newcache()
	maxCacheSize = 1 * 1024 * 1024 //Max 1MB objects will be cached
	folder       = "/Public"
)

type cache struct {
	*sync.RWMutex
	data map[string]*cacheobj
}

func newcache() *cache {
	return &cache{&sync.RWMutex{}, make(map[string]*cacheobj)}
}

func (c *cache) Get(key string) (*cacheobj, error) {
	c.RLock()
	defer c.RUnlock()
	obj, ok := c.data[key]
	if ok {
		return obj, nil
	}
	return nil, errNotCached
}

func (c *cache) Set(key string, obj *cacheobj) error {
	c.Lock()
	defer c.Unlock()
	c.data[key] = obj
	return nil
}

type cacheobj struct {
	data        []byte    //Body
	lastmod     time.Time //Last modified time
	etag        string    //Etag
	lastFetch   time.Time //Last time we detched this object from Dropbox
	contentType string    //Content-Type
	exists      bool      //Used to cache 404
	entry       *dropbox.Entry
}

func longpollloop() {
	for {
		err := longpoll()
		if err != nil {
			log.Println(err)
			//Backoff a bit
			time.Sleep(time.Minute)
		}
	}
}

//Longpoll public folder and invalidate all caches if anything changed...
func longpoll() error {
	cur, err := db.LatestCursor(folder, false)
	if err != nil {
		return err
	}
	//log.Println(cur)
	dp, err := db.LongPollDelta(cur.Cursor, 300)
	if err != nil {
		return err
	}
	//change <- true
	log.Println("Invalidating")
	lmod = time.Now()
	time.Sleep(time.Second * time.Duration(dp.Backoff))
	return nil
}

//dbhandlerNotFound caches 404s so we dont keep spamming dropbox.
//Pretty cheap
func dbhandlerNotFound(w http.ResponseWriter, r *http.Request, key string) {
	obj := &cacheobj{
		lastFetch: time.Now(),
		exists:    false,
	}
	dbcache.Set(key, obj)
	dbhandlerServe(w, r, obj)
}

func dbhandlerMiss(w http.ResponseWriter, r *http.Request, key string, oldobj *cacheobj) {
	//Fetch from dropbox, make obj
	entry, err := db.Metadata((folder + key), false, false, "", "", 1)
	if err != nil {
		log.Println(err)
		httperr, ok := err.(*dropbox.Error)
		if ok && httperr.StatusCode == http.StatusNotFound {
			//Create 404 obj and serve.
			dbhandlerNotFound(w, r, key)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	//If directory serve 404
	if entry.IsDir {
		dbhandlerNotFound(w, r, key)
		return
	}
	//We have entry, and no errors... so far...
	obj := &cacheobj{
		lastFetch: time.Now(),
		exists:    true,
		entry:     entry,
	}
	//If oldobj is still valid, reuse it instead of fetch again...
	if oldobj != nil {
		//oldobj was not 404
		if oldobj.entry != nil {
			//oldobj is same version as obj
			if oldobj.entry.Revision == obj.entry.Revision {
				obj.data = oldobj.data
				obj.entry.MimeType = oldobj.entry.MimeType
				dbcache.Set(key, obj)
				dbhandlerServe(w, r, obj)
				return
			}
		}
	}

	rd, _, err := db.Download(folder+key, "", 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rd.Close()
	//TODO: if the file is larger than maxCacheSize, then bypass cache and copy reader to writer
	obj.data, err = ioutil.ReadAll(rd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	//Fix mime type - for when dropbox does not detect
	//Dropbox does not have correct mime for json!
	if obj.entry.MimeType == "application/octet-stream" {
		s := strings.Split(key, ".")
		if len(s) > 1 {
			ext := "." + s[len(s)-1]
			mtype := mime.TypeByExtension(ext)
			if mtype != "" {
				obj.entry.MimeType = mtype
			}
		}
	}
	dbcache.Set(key, obj)
	dbhandlerServe(w, r, obj)
}

//Serve object from cache
func dbhandlerServe(w http.ResponseWriter, r *http.Request, obj *cacheobj) {
	if !obj.exists {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", obj.entry.MimeType)
	w.Header().Set("etag", obj.entry.Revision)
	mtime := time.Time(obj.entry.Modified)
	w.Header().Set("last-modified", mtime.Format(http.TimeFormat))
	//TODO: How to manage cache-controls.... should we do it?
	//TODO: See conditional request headers and 304 if needed
	w.Write(obj.data)
}

func dbhandler(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path
	//TODO: Do we need to validate anything in the path?
	obj, err := dbcache.Get(key)
	if err == errNotCached {
		//goto cache miss
		dbhandlerMiss(w, r, key, nil)
		return
	}
	if err != nil {
		//Return fail...
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	//Check lastfetched
	if obj.lastFetch.Before(lmod) {
		//goto cache miss
		dbhandlerMiss(w, r, key, obj)
		return
	}
	//So... we have an obj...
	dbhandlerServe(w, r, obj)
}

func main() {
	hostname := flag.String("hostname", "", "if present it will serve on https using autocert")
	flag.StringVar(&folder, "folder", "/Public", "The dropbox folder to serve from")
	flag.Parse()

	db = dropbox.NewDropbox()
	db.SetAppInfo(os.Getenv("CLIENT_ID"), os.Getenv("CLIENT_SECRET"))
	db.SetAccessToken(os.Getenv("ACCESS_TOKEN"))
	go longpollloop()
	//http.HandleFunc("/", dbhandler)
	if *hostname != "" {
		m := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(*hostname),
		}
		s := &http.Server{
			Addr:           ":https",
			TLSConfig:      &tls.Config{GetCertificate: m.GetCertificate},
			Handler:        gziphandler.GzipHandler(http.HandlerFunc(dbhandler)),
			ReadTimeout:    10 * time.Second,
			WriteTimeout:   10 * time.Second,
			MaxHeaderBytes: 1 << 20,
		}
		log.Println("Listening on :https")
		//TODO: If we are listening on https, then maybe we should listen and redirect http to https also...
		log.Fatal(s.ListenAndServeTLS("", ""))
	} else {
		s := &http.Server{
			Addr:           ":8889",
			Handler:        gziphandler.GzipHandler(http.HandlerFunc(dbhandler)),
			ReadTimeout:    10 * time.Second,
			WriteTimeout:   10 * time.Second,
			MaxHeaderBytes: 1 << 20,
		}
		log.Println("Listening on :8889")
		log.Fatal(s.ListenAndServe())
	}
}
