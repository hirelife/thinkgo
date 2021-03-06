// Copyright 2016 HenryLee. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// HTTP file system with cache request handler

package thinkgo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/henrylee2cn/thinkgo/acceptencoder"
	"github.com/henrylee2cn/thinkgo/freecache"
)

const indexPage = "/index.html"

// File cache system manager
type FileServerManager struct {
	files             map[string]CacheFile
	cache             *freecache.Cache
	fileExpireSeconds int
	maxSizeOfSingle   int64
	enableCache       bool
	enableGzip        bool
	errorFunc         ErrorFunc
	filesLock         sync.RWMutex
}

// The cache size will be set to 512KB at minimum.
// If the size is set relatively large, you should call
// `debug.SetGCPercent()`, set it to a much smaller value
// to limit the memory consumption and GC pause time.
// expireSeconds <= 0 means no expire.
func newFileServerManager(cacheSize int64, fileExpireSeconds int, enableCache bool, enableGzip bool) *FileServerManager {
	manager := new(FileServerManager)
	manager.fileExpireSeconds = fileExpireSeconds
	manager.cache = freecache.NewCache(int(cacheSize))
	manager.files = map[string]CacheFile{}
	manager.maxSizeOfSingle = cacheSize / 1024
	if manager.maxSizeOfSingle < 512 {
		manager.maxSizeOfSingle = 512
	}
	manager.enableCache = enableCache
	manager.enableGzip = enableGzip
	return manager
}

// Gets or stores the cache file without compression.
func (c *FileServerManager) OpenFile(name string) (http.File, error) {
	f, err := c.Get(name)
	if err == nil {
		return f, nil
	}
	f, err = os.Open(name)
	if err != nil {
		return nil, err
	}
	fileInfo, err := f.Stat()
	if err != nil || fileInfo.IsDir() {
		return f, err
	}
	// If the name is larger than 65535 or body is larger than 1/1024 of the cache size,
	// the entry will not be written to the cache. expireSeconds <= 0 means no expire,
	// but it can be evicted when cache is full.
	if fileInfo.Size() <= c.maxSizeOfSingle {
		var content []byte
		content, err = ioutil.ReadAll(f)
		defer f.Close()
		if err != nil {
			return nil, err
		}
		return c.Set(name, content, fileInfo, "")
	}
	return f, err
}

func (c *FileServerManager) Open(ctx *Context, name string, fs http.FileSystem) (http.File, error) {
	f, err := c.Get(name)
	if err == nil {
		if encoding := f.(*CacheFile).encoding; encoding != "" {
			ctx.W.Header().Set("Content-Encoding", encoding)
		}
		return f, nil
	}
	f, err = fs.Open(name)
	if err != nil {
		return nil, err
	}
	fileInfo, err := f.Stat()
	if err != nil || fileInfo.IsDir() {
		return f, err
	}
	var content []byte
	var encoding string
	if c.enableGzip {
		content, encoding, err = readWithCompress(ctx, f)
		defer f.Close()
		if err != nil {
			return nil, err
		}
	} else {
		content, err = ioutil.ReadAll(f)
		defer f.Close()
		if err != nil {
			return nil, err
		}
	}
	// If the name is larger than 65535 or body is larger than 1/1024 of the cache size,
	// the entry will not be written to the cache. expireSeconds <= 0 means no expire,
	// but it can be evicted when cache is full.
	if int64(len(content)) <= c.maxSizeOfSingle {
		return c.Set(name, content, fileInfo, encoding)
	}
	return f, err
}

func (c *FileServerManager) Get(name string) (http.File, error) {
	b, err := c.cache.Get([]byte(name))
	if err != nil {
		c.filesLock.Lock()
		delete(c.files, name)
		c.filesLock.Unlock()
		return nil, err
	}
	c.filesLock.RLock()
	f := c.files[name]
	c.filesLock.RUnlock()
	f.Reader = bytes.NewReader(b)
	return &f, nil
}

func (c *FileServerManager) Set(name string, body []byte, fileInfo os.FileInfo, encoding string) (http.File, error) {
	err := c.cache.Set([]byte(name), body, c.fileExpireSeconds)
	if err != nil {
		return nil, err
	}
	f := CacheFile{
		fileInfo: fileInfo,
		encoding: encoding,
	}
	c.filesLock.Lock()
	c.files[name] = f
	c.filesLock.Unlock()
	f.Reader = bytes.NewReader(body)
	return &f, nil
}

type CacheFile struct {
	fileInfo os.FileInfo
	encoding string
	*bytes.Reader
}

var _ http.File = new(CacheFile)

func (c *CacheFile) Stat() (os.FileInfo, error) {
	return c.fileInfo, nil
}

func (c *CacheFile) Close() error {
	c.Reader = nil
	c.fileInfo = nil
	return nil
}

func (c *CacheFile) Readdir(count int) ([]os.FileInfo, error) {
	return []os.FileInfo{}, errors.New("Readdir " + c.fileInfo.Name() + ": The system cannot find the path specified.")
}

func (c *FileServerManager) dirList(ctx *Context, f http.File) {
	dirs, err := f.Readdir(-1)
	if err != nil {
		// TODO: log err.Error() to the Server.ErrorLog, once it's possible
		// for a handler to get at its Server via the *Context. See
		// Issue 12438.
		Global.errorFunc(ctx, "Error reading directory", http.StatusInternalServerError)
		return
	}
	sort.Sort(byName(dirs))

	ctx.W.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(ctx.W, "<pre>\n")
	for _, d := range dirs {
		name := d.Name()
		if d.IsDir() {
			name += "/"
		}
		// name may contain '?' or '#', which must be escaped to remain
		// part of the URL path, and not indicate the start of a query
		// string or fragment.
		url := url.URL{Path: name}
		fmt.Fprintf(ctx.W, "<a href=\"%s\">%s</a>\n", url.String(), htmlReplacer.Replace(name))
	}
	fmt.Fprintf(ctx.W, "</pre>\n")
}

var htmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	// "&#34;" is shorter than "&quot;".
	`"`, "&#34;",
	// "&#39;" is shorter than "&apos;" and apos was not in HTML until HTML5.
	"'", "&#39;",
)

// ServeContent replies to the request using the content in the
// provided ReadSeeker. The main benefit of ServeContent over io.Copy
// is that it handles Range requests properly, sets the MIME type, and
// handles If-Modified-Since requests.
//
// If the response's Content-Type header is not set, ServeContent
// first tries to deduce the type from name's file extension and,
// if that fails, falls back to reading the first block of the content
// and passing it to DetectContentType.
// The name is otherwise unused; in particular it can be empty and is
// never sent in the response.
//
// If modtime is not the zero time or Unix epoch, ServeContent
// includes it in a Last-Modified header in the response. If the
// request includes an If-Modified-Since header, ServeContent uses
// modtime to decide whether the content needs to be sent at all.
//
// The content's Seek method must work: ServeContent uses
// a seek to the end of the content to determine its size.
//
// If the caller has set ctx's ETag header, ServeContent uses it to
// handle requests using If-Range and If-None-Match.
//
// Note that *os.File implements the io.ReadSeeker interface.
func (c *FileServerManager) ServeContent(ctx *Context, name string, modtime time.Time, content io.ReadSeeker) {
	sizeFunc := func() (int64, error) {
		size, err := content.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, errSeeker
		}
		_, err = content.Seek(0, io.SeekStart)
		if err != nil {
			return 0, errSeeker
		}
		return size, nil
	}
	c.serveContent(ctx, name, modtime, sizeFunc, content)
}

// errSeeker is returned by ServeContent's sizeFunc when the content
// doesn't seek properly. The underlying Seeker's error text isn't
// included in the sizeFunc reply so it's not sent over HTTP to end
// users.
var errSeeker = errors.New("seeker can't seek")

// The algorithm uses at most sniffLen bytes to make its decision.
const sniffLen = 512

// if name is empty, filename is unknown. (used for mime type, before sniffing)
// if modtime.IsZero(), modtime is unknown.
// content must be seeked to the beginning of the file.
// The sizeFunc is called at most once. Its error, if any, is sent in the HTTP response.
func (c *FileServerManager) serveContent(ctx *Context, name string, modtime time.Time, sizeFunc func() (int64, error), content io.ReadSeeker) {
	if checkLastModified(ctx, modtime) {
		return
	}
	rangeReq, done := checkETag(ctx, modtime)
	if done {
		return
	}

	code := http.StatusOK

	// If Content-Type isn't set, use the file's extension to find it, but
	// if the Content-Type is unset explicitly, do not sniff the type.
	ctypes, haveType := ctx.W.Header()["Content-Type"]
	var ctype string
	if !haveType {
		ctype = mime.TypeByExtension(filepath.Ext(name))
		if ctype == "" {
			// read a chunk to decide between utf-8 text and binary
			var buf [sniffLen]byte
			n, _ := io.ReadFull(content, buf[:])
			ctype = http.DetectContentType(buf[:n])
			_, err := content.Seek(0, io.SeekStart) // rewind to output whole file
			if err != nil {
				Global.errorFunc(ctx, "seeker can't seek", http.StatusInternalServerError)
				return
			}
		}
		ctx.W.Header().Set("Content-Type", ctype)
	} else if len(ctypes) > 0 {
		ctype = ctypes[0]
	}

	size, err := sizeFunc()
	if err != nil {
		Global.errorFunc(ctx, err.Error(), http.StatusInternalServerError)
		return
	}

	// handle Content-Range header.
	sendSize := size
	var sendContent io.Reader = content
	if size >= 0 {
		ranges, err := parseRange(rangeReq, size)
		if err != nil {
			Global.errorFunc(ctx, err.Error(), http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if sumRangesSize(ranges) > size {
			// The total number of bytes in all the ranges
			// is larger than the size of the file by
			// itself, so this is probably an attack, or a
			// dumb client. Ignore the range request.
			ranges = nil
		}
		switch {
		case len(ranges) == 1:
			// RFC 2616, Section 14.16:
			// "When an HTTP message includes the content of a single
			// range (for example, a response to a request for a
			// single range, or to a request for a set of ranges
			// that overlap without any holes), this content is
			// transmitted with a Content-Range header, and a
			// Content-Length header showing the number of bytes
			// actually transferred.
			// ...
			// A response to a request for a single range MUST NOT
			// be sent using the multipart/byteranges media type."
			ra := ranges[0]
			if _, err := content.Seek(ra.start, io.SeekStart); err != nil {
				Global.errorFunc(ctx, err.Error(), http.StatusRequestedRangeNotSatisfiable)
				return
			}
			sendSize = ra.length
			code = http.StatusPartialContent
			ctx.W.Header().Set("Content-Range", ra.contentRange(size))
		case len(ranges) > 1:
			sendSize = rangesMIMESize(ranges, ctype, size)
			code = http.StatusPartialContent

			pr, pw := io.Pipe()
			mw := multipart.NewWriter(pw)
			ctx.W.Header().Set("Content-Type", "multipart/byteranges; boundary="+mw.Boundary())
			sendContent = pr
			defer pr.Close() // cause writing goroutine to fail and exit if CopyN doesn't finish.
			go func() {
				for _, ra := range ranges {
					part, err := mw.CreatePart(ra.mimeHeader(ctype, size))
					if err != nil {
						pw.CloseWithError(err)
						return
					}
					if _, err := content.Seek(ra.start, io.SeekStart); err != nil {
						pw.CloseWithError(err)
						return
					}
					if _, err := io.CopyN(part, content, ra.length); err != nil {
						pw.CloseWithError(err)
						return
					}
				}
				mw.Close()
				pw.Close()
			}()
		}

		ctx.W.Header().Set("Accept-Ranges", "bytes")
		if ctx.W.Header().Get("Content-Encoding") == "" {
			ctx.W.Header().Set("Content-Length", strconv.FormatInt(sendSize, 10))
		}
	}

	ctx.W.WriteHeader(code)

	if ctx.R.Method != "HEAD" {
		io.CopyN(ctx.W, sendContent, sendSize)
	}
}

var unixEpochTime = time.Unix(0, 0)

// modtime is the modification time of the resource to be served, or IsZero().
// return value is whether this request is now complete.
func checkLastModified(ctx *Context, modtime time.Time) bool {
	if modtime.IsZero() || modtime.Equal(unixEpochTime) {
		// If the file doesn't have a modtime (IsZero), or the modtime
		// is obviously garbage (Unix time == 0), then ignore modtimes
		// and don't process the If-Modified-Since header.
		return false
	}
	// The Date-Modified header truncates sub-second precision, so
	// use mtime < t+1s instead of mtime <= t to check for unmodified.
	if t, err := time.Parse(http.TimeFormat, ctx.R.Header.Get("If-Modified-Since")); err == nil && modtime.Before(t.Add(1*time.Second)) {
		h := ctx.W.Header()
		delete(h, "Content-Type")
		delete(h, "Content-Length")
		ctx.W.WriteHeader(http.StatusNotModified)
		return true
	}
	ctx.W.Header().Set("Last-Modified", modtime.UTC().Format(http.TimeFormat))
	return false
}

// checkETag implements If-None-Match and If-Range checks.
//
// The ETag or modtime must have been previously set in the
// *Context's headers. The modtime is only compared at second
// granularity and may be the zero value to mean unknown.
//
// The return value is the effective request "Range" header to use and
// whether this request is now considered done.
func checkETag(ctx *Context, modtime time.Time) (rangeReq string, done bool) {
	etag := ctx.W.Header().Get("Etag")
	rangeReq = ctx.R.Header.Get("Range")

	// Invalidate the range request if the entity doesn't match the one
	// the client was expecting.
	// "If-Range: version" means "ignore the Range: header unless version matches the
	// current file."
	// We only support ETag versions.
	// The caller must have set the ETag on the response already.
	if ir := ctx.R.Header.Get("If-Range"); ir != "" && ir != etag {
		// The If-Range value is typically the ETag value, but it may also be
		// the modtime date. See golang.org/issue/8367.
		timeMatches := false
		if !modtime.IsZero() {
			if t, err := http.ParseTime(ir); err == nil && t.Unix() == modtime.Unix() {
				timeMatches = true
			}
		}
		if !timeMatches {
			rangeReq = ""
		}
	}

	if inm := ctx.R.Header.Get("If-None-Match"); inm != "" {
		// Must know ETag.
		if etag == "" {
			return rangeReq, false
		}

		// TODO(bradfitz): non-GET/HEAD requests require more work:
		// sending a different status code on matches, and
		// also can't use weak cache validators (those with a "W/
		// prefix).  But most users of ServeContent will be using
		// it on GET or HEAD, so only support those for now.
		if ctx.R.Method != "GET" && ctx.R.Method != "HEAD" {
			return rangeReq, false
		}

		// TODO(bradfitz): deal with comma-separated or multiple-valued
		// list of If-None-match values. For now just handle the common
		// case of a single item.
		if inm == etag || inm == "*" {
			h := ctx.W.Header()
			delete(h, "Content-Type")
			delete(h, "Content-Length")
			ctx.W.WriteHeader(http.StatusNotModified)
			return "", true
		}
	}
	return rangeReq, false
}

// name is '/'-separated, not filepath.Separator.
func (c *FileServerManager) serveFile(ctx *Context, fs http.FileSystem, name string, redirect bool) {

	// redirect .../index.html to .../
	// can't use Redirect() because that would make the path absolute,
	// which would be a problem running under StripPrefix
	if strings.HasSuffix(ctx.R.URL.Path, indexPage) {
		localRedirect(ctx, "./")
		return
	}
	var f http.File
	var err error
	if c.enableCache {
		f, err = c.Open(ctx, name, fs)
	} else {
		f, err = fs.Open(name)
		if err == nil && c.enableGzip {
			f, err = compressFile(ctx, f)
		}
	}
	if err != nil {
		msg, code := toHTTPError(err)
		Global.errorFunc(ctx, msg, code)
		return
	}
	defer f.Close()

	d, err := f.Stat()
	if err != nil {
		msg, code := toHTTPError(err)
		Global.errorFunc(ctx, msg, code)
		return
	}

	if redirect {
		// redirect to canonical path: / at end of directory url
		// ctx.R.URL.Path always begins with /
		url := ctx.R.URL.Path
		if d.IsDir() {
			if url[len(url)-1] != '/' {
				localRedirect(ctx, path.Base(url)+"/")
				return
			}
		} else {
			if url[len(url)-1] == '/' {
				localRedirect(ctx, "../"+path.Base(url))
				return
			}
		}
	}

	// redirect if the directory name doesn't end in a slash
	if d.IsDir() {
		url := ctx.R.URL.Path
		if url[len(url)-1] != '/' {
			localRedirect(ctx, path.Base(url)+"/")
			return
		}
	}

	// use contents of index.html for directory, if present
	if d.IsDir() {
		index := strings.TrimSuffix(name, "/") + indexPage
		var ff http.File
		var err error
		if c.enableCache {
			ff, err = c.Open(ctx, index, fs)
		} else {
			ff, err = fs.Open(index)
			if err == nil && c.enableGzip {
				ff, err = compressFile(ctx, ff)
			}
		}
		if err == nil {
			defer ff.Close()
			dd, err := ff.Stat()
			if err == nil {
				name = index
				d = dd
				f = ff
			}
		}
	}

	// Still a directory? (we didn't find an index.html file)
	if d.IsDir() {
		if checkLastModified(ctx, d.ModTime()) {
			return
		}
		c.dirList(ctx, f)
		return
	}

	// serveContent will check modification time
	sizeFunc := func() (int64, error) { return d.Size(), nil }
	c.serveContent(ctx, d.Name(), d.ModTime(), sizeFunc, f)
}

func readWithCompress(ctx *Context, file http.File) ([]byte, string, error) {
	var buf = &bytes.Buffer{}
	var encoding string
	if b, n, _ := acceptencoder.WriteFile(acceptencoder.ParseEncoding(ctx.R), buf, file); b {
		ctx.W.Header().Set("Content-Encoding", n)
		encoding = n
	}
	return buf.Bytes(), encoding, nil
}

func compressFile(ctx *Context, f http.File) (http.File, error) {
	content, _, err := readWithCompress(ctx, f)
	defer f.Close()
	if err != nil {
		return nil, err
	}
	fileInfo, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return &CacheFile{
		fileInfo: fileInfo,
		Reader:   bytes.NewReader(content),
	}, nil
}

// toHTTPError returns a non-specific HTTP error message and status code
// for a given non-nil error value. It's important that toHTTPError does not
// actually return err.Error(), since msg and httpStatus are returned to users,
// and historically Go's ServeContent always returned just "404 Not Found" for
// all errors. We don't want to start leaking information in error messages.
func toHTTPError(err error) (msg string, httpStatus int) {
	if os.IsNotExist(err) {
		return "404 page not found", http.StatusNotFound
	}
	if os.IsPermission(err) {
		return "403 Forbidden", http.StatusForbidden
	}
	// Default:
	return "500 Internal Server Error", http.StatusInternalServerError
}

// localRedirect gives a Moved Permanently response.
// It does not convert relative paths to absolute paths like Redirect does.
func localRedirect(ctx *Context, newPath string) {
	if q := ctx.R.URL.RawQuery; q != "" {
		newPath += "?" + q
	}
	ctx.W.Header().Set("Location", newPath)
	ctx.W.WriteHeader(http.StatusMovedPermanently)
}

// ServeFile replies to the request with the contents of the named
// file or directory.
//
// If the provided file or directory name is a relative path, it is
// interpreted relative to the current directory and may ascend to parent
// directories. If the provided name is constructed from user input, it
// should be sanitized before calling ServeFile. As a precaution, ServeFile
// will reject requests where r.URL.Path contains a ".." path element.
//
// As a special case, ServeFile redirects any request where r.URL.Path
// ends in "/index.html" to the same path, without the final
// "index.html". To avoid such redirects either modify the path or
// use ServeContent.
func (c *FileServerManager) ServeFile(ctx *Context, name string) {
	if containsDotDot(ctx.R.URL.Path) {
		// Too many programs use ctx.R.URL.Path to construct the argument to
		// serveFile. Reject the request under the assumption that happened
		// here and ".." may not be wanted.
		// Note that name might not contain "..", for example if code (still
		// incorrectly) used filepath.Join(myDir, ctx.R.URL.Path).
		Global.errorFunc(ctx, "invalid URL path", http.StatusBadRequest)
		return
	}
	dir, file := filepath.Split(name)
	c.serveFile(ctx, http.Dir(dir), file, false)
}

func containsDotDot(v string) bool {
	if !strings.Contains(v, "..") {
		return false
	}
	for _, ent := range strings.FieldsFunc(v, isSlashRune) {
		if ent == ".." {
			return true
		}
	}
	return false
}

func isSlashRune(r rune) bool { return r == '/' || r == '\\' }

type fileHandler struct {
	root              http.FileSystem
	fileServerManager *FileServerManager
}

// http.FileServer returns a handler that serves HTTP requests
// with the contents of the file system rooted at root.
//
// To use the operating system's file system implementation,
// use http.Dir:
//
//     http.Handle("/", http.FileServer(http.Dir("/tmp")))
//
// As a special case, the returned file server redirects any request
// ending in "/index.html" to the same path, without the final
// "index.html".
func (c *FileServerManager) FileServer(root http.FileSystem) Handler {
	return &fileHandler{root, c}
}

func (f *fileHandler) Serve(ctx *Context) error {
	r := ctx.R
	upath := r.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
		r.URL.Path = upath
	}
	f.fileServerManager.serveFile(ctx, f.root, path.Clean(upath), true)
	return nil
}

// httpRange specifies the byte range to be sent to the client.
type httpRange struct {
	start, length int64
}

func (r httpRange) contentRange(size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, size)
}

func (r httpRange) mimeHeader(contentType string, size int64) textproto.MIMEHeader {
	return textproto.MIMEHeader{
		"Content-Range": {r.contentRange(size)},
		"Content-Type":  {contentType},
	}
}

// parseRange parses a Range header string as per RFC 2616.
func parseRange(s string, size int64) ([]httpRange, error) {
	if s == "" {
		return nil, nil // header not present
	}
	const b = "bytes="
	if !strings.HasPrefix(s, b) {
		return nil, errors.New("invalid range")
	}
	var ranges []httpRange
	for _, ra := range strings.Split(s[len(b):], ",") {
		ra = strings.TrimSpace(ra)
		if ra == "" {
			continue
		}
		i := strings.Index(ra, "-")
		if i < 0 {
			return nil, errors.New("invalid range")
		}
		start, end := strings.TrimSpace(ra[:i]), strings.TrimSpace(ra[i+1:])
		var r httpRange
		if start == "" {
			// If no start is specified, end specifies the
			// range start relative to the end of the file.
			i, err := strconv.ParseInt(end, 10, 64)
			if err != nil {
				return nil, errors.New("invalid range")
			}
			if i > size {
				i = size
			}
			r.start = size - i
			r.length = size - r.start
		} else {
			i, err := strconv.ParseInt(start, 10, 64)
			if err != nil || i >= size || i < 0 {
				return nil, errors.New("invalid range")
			}
			r.start = i
			if end == "" {
				// If no end is specified, range extends to end of the file.
				r.length = size - r.start
			} else {
				i, err := strconv.ParseInt(end, 10, 64)
				if err != nil || r.start > i {
					return nil, errors.New("invalid range")
				}
				if i >= size {
					i = size - 1
				}
				r.length = i - r.start + 1
			}
		}
		ranges = append(ranges, r)
	}
	return ranges, nil
}

// countingWriter counts how many bytes have been written to it.
type countingWriter int64

func (w *countingWriter) Write(p []byte) (n int, err error) {
	*w += countingWriter(len(p))
	return len(p), nil
}

// rangesMIMESize returns the number of bytes it takes to encode the
// provided ranges as a multipart response.
func rangesMIMESize(ranges []httpRange, contentType string, contentSize int64) (encSize int64) {
	var w countingWriter
	mw := multipart.NewWriter(&w)
	for _, ra := range ranges {
		mw.CreatePart(ra.mimeHeader(contentType, contentSize))
		encSize += ra.length
	}
	mw.Close()
	encSize += int64(w)
	return
}

func sumRangesSize(ranges []httpRange) (size int64) {
	for _, ra := range ranges {
		size += ra.length
	}
	return
}

type byName []os.FileInfo

func (s byName) Len() int           { return len(s) }
func (s byName) Less(i, j int) bool { return s[i].Name() < s[j].Name() }
func (s byName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
