package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dropbox/dropbox-sdk-go-unofficial/dropbox"
	"github.com/dropbox/dropbox-sdk-go-unofficial/dropbox/files"
	"github.com/nytimes/gziphandler"
	"golang.org/x/crypto/acme/autocert"
)

var (
	db           files.Client
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
	entry       *files.FileMetadata
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
	lfopt := files.NewListFolderArg(folder)
	lfopt.Recursive = true
	cur, err := db.ListFolderGetLatestCursor(lfopt)
	if err != nil {
		return err
	}
	//log.Println(cur)ListFolderLongpollArg
	dp, err := db.ListFolderLongpoll(&files.ListFolderLongpollArg{Cursor: cur.Cursor, Timeout: 300})
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
	tmp, err := db.GetMetadata(files.NewGetMetadataArg(folder + key))
	if err != nil {
		log.Println(err)
		httperr, ok := err.(files.GetMetadataAPIError)
		if ok && strings.Contains(httperr.APIError.Error(), "not_found") {
			//Create 404 obj and serve.
			dbhandlerNotFound(w, r, key)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	entry, ok := tmp.(*files.FileMetadata)
	if !ok {
		dbhandlerNotFound(w, r, key)
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
			if oldobj.entry.Rev == obj.entry.Rev {
				obj.data = oldobj.data
				obj.contentType = oldobj.contentType
				//obj.entry.MimeType = oldobj.entry.MimeType
				dbcache.Set(key, obj)
				dbhandlerServe(w, r, obj)
				return
			}
		}
	}
	var rd io.ReadCloser
	obj.entry, rd, err = db.Download(files.NewDownloadArg(folder + key))
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
	obj.contentType = "application/octet-stream"
	s := strings.Split(key, ".")
	if len(s) > 1 {
		ext := "." + s[len(s)-1]
		mtype := mime.TypeByExtension(ext)
		if mtype != "" {
			obj.contentType = mtype
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
	w.Header().Set("Content-Type", obj.contentType)
	w.Header().Set("etag", obj.entry.Rev)
	mtime := obj.entry.ServerModified
	w.Header().Set("last-modified", mtime.Format(http.TimeFormat))
	//See conditional request headers and 304 if needed
	if r.Header.Get("If-None-Match") == obj.entry.Rev {
		//Our cached version matches the one user has cached.
		w.WriteHeader(http.StatusNotModified)
		return
	}
	//TODO: How to manage cache-controls.... should we do it?
	w.Write(obj.data)
}

func dbhandler(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path
	//Add a robots.txt . We dont want google to index
	if r.URL.Path == "/robots.txt" {
		w.Write([]byte(`User-agent: *
Disallow: /
`))
		return
	} else if r.URL.Path == "/" {
		//Redirect root page to git repo . Shameless plug :)
		http.Redirect(w, r, "https://github.com/sajal/dboxserver", http.StatusFound)
		return
	}
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
	config := dropbox.Config{Token: os.Getenv("ACCESS_TOKEN"), Verbose: false} // second arg enables verbose logging in the SDK
	db = files.New(config)
	//db = dropbox.NewDropbox()
	//db.SetAppInfo(os.Getenv("CLIENT_ID"), os.Getenv("CLIENT_SECRET"))
	//db.SetAccessToken(os.Getenv("ACCESS_TOKEN"))
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
