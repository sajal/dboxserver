# dboxserver
Serve files from dropbox over http(s)

## Background

Since October 3, 2016 Dropbox [stopped serving html files](https://www.dropbox.com/help/16) with the correct `text/html` content-type header. `.html` and `.json` files are now served with a `content-disposition` header which makes the browser download the file instead of render it.

This created issues for me since I am used to storing output of various scripts into my `/Public` directory and view them on desktop/mobile and/or share them with others.

## Solution

dboxserver uses Dropbox API to fetch and serve files over http(s).

check out [https://db.sajalkayan.com/hello.html](https://db.sajalkayan.com/hello.html)

## Usage

	CLIENT_ID="REMOVED" CLIENT_SECRET="REMOVED" ACCESS_TOKEN="REMOVED" go run server.go -hostname "db.sajalkayan.com"

You need to create an app at the [Dropbox developer portal](https://www.dropbox.com/developers). 
`CLIENT_ID` - "App key"
`CLIENT_SECRET` - "App secret"
`ACCESS_TOKEN` - Allow implicit grant and generate an access token.
`-hostname` - If configured the server listens over https on :443 and gets certificate from Let's Encrypt otherwise it listens over http on :8889.
`folder` - Defaults to `/Public` . The Dropbox folder you want to expose.

## Features

1. Caches objects in memory forever.
2. Invalidates cache as soon as anything is changed in the monitored folder.
3. Only cache objects lower than specified size (not yet implemented).
4. Tries to fix content-type if Dropbox falls back to `application/octet-stream` - example for json

## TODO

1. Code cleanup - Currently this is result of couple of hours hack.
2. Test cases
3. Bug fixes