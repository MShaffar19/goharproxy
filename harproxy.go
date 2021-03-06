package goharproxy

import (
	"net"
	"net/http"
	"sync"
	"log"
	"strconv"
	"io"
	"strings"
	"regexp"
	"fmt"
	"encoding/json"
	"bytes"
	"io/ioutil"
	"time"
	"github.com/quantum/goproxy"
	"github.com/quantum/goproxy/transport"
)

// HarProxy

var Verbosity bool

type HarProxy struct {
	// Our go proxy
	Proxy *goproxy.ProxyHttpServer

	// The port our proxy is listening on
	Port int

	// Our HAR log.
	// Starting size of 1000 entries, enlarged if necessary
	// Read the specification here: http://www.softwareishard.com/blog/har-12-spec/
	HarLog *HarLog

	// Stoppable listener - used to stop http proxy
	StoppableListener *stoppableListener

	// This channel is used to signal when the http.Serve function is done serving our proxy
	isDone chan bool

	// Stores hosts we want to redirect to a different ip / host
	hostEntries []ProxyHosts


	// We use this channel to receive a request and response from the proxy.
	// We don't separate this into 2 channels because we want the specific request for our response
	// to arrive at the same time.
	entryChannel chan reqAndResp

	// This is the count of entries we are currently waiting to finish processing
	entriesInProcess int
}

func orPanic(err error) {
	if err != nil {
		panic(err)
	}
}

type stoppableListener struct {
	net.Listener
	sync.WaitGroup
}


func newStoppableListener(l net.Listener) *stoppableListener {
	return &stoppableListener{l, sync.WaitGroup{}}
}

func NewHarProxy() *HarProxy {
	return NewHarProxyWithPort(0)
}

func NewHarProxyWithPort(port int) *HarProxy {
	harProxy := HarProxy {
		Proxy 			 : goproxy.NewProxyHttpServer(),
		Port 			 : port,
		HarLog 			 : newHarLog(),
		hostEntries 	 : make([]ProxyHosts, 0, 100),
		isDone 			 : make(chan bool),
		entryChannel	 : make(chan reqAndResp),
		entriesInProcess : 0,
	}
	createProxy(&harProxy)
	return &harProxy
}

type reqAndResp struct {
	req 	*http.Request
	start 	 time.Time
	resp 	*http.Response
	end   	 time.Time
}

func createProxy(proxy *HarProxy) {
	tr := transport.Transport{Proxy: transport.ProxyFromEnvironment}
	proxy.Proxy.Verbose = Verbosity
	go processEntriesFunc(proxy)
	proxy.Proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		reqAndResp := new(reqAndResp)
		reqAndResp.start = time.Now()
		if captureContent && req.ContentLength > 0 {
			req, reqAndResp.req = copyReq(req)
		} else {
			reqAndResp.req = req
		}
		ctx.RoundTripper = goproxy.RoundTripperFunc(func (req *http.Request, ctx *goproxy.ProxyCtx) (resp *http.Response, err error) {
			reqAndResp.end = time.Now()
			ctx.UserData, resp, err = tr.DetailedRoundTrip(req)
			if captureContent && resp.ContentLength > 0 {
				resp, reqAndResp.resp = copyResp(resp)
			} else {
				reqAndResp.resp = resp
			}
			proxy.entryChannel<- *reqAndResp
			return resp, err
		})
		return handleRequest(req, proxy)
	})
}

func copyReq(req *http.Request) (*http.Request, *http.Request) {
	reqCopy := new(http.Request)
	*reqCopy = *req
	req.Body, reqCopy.Body = copyReadCloser(req.Body, req.ContentLength)
	return req, reqCopy
}

func copyResp(resp *http.Response) (*http.Response, *http.Response) {
	respCopy := new(http.Response)
	*respCopy = *resp
	resp.Body, respCopy.Body = copyReadCloser(resp.Body, resp.ContentLength)
	return resp, respCopy
}

func copyReadCloser(readCloser io.ReadCloser, len int64) (io.ReadCloser, io.ReadCloser) {
	temp := bytes.NewBuffer(make([]byte, 0, len))
	teeReader := io.TeeReader(readCloser, temp)
	copy := bytes.NewBuffer(make([]byte, 0, len))
	copy.ReadFrom(teeReader)
	return ioutil.NopCloser(temp), ioutil.NopCloser(copy)
}

func processEntriesFunc(proxy *HarProxy) {
	for {
		reqAndResp ,ok := <-proxy.entryChannel
		if !ok {
			log.Println("GOT DONE SIGNAL")
			break
		}
		proxy.entriesInProcess += 1
		go func() {
			harEntry := new(HarEntry)
			harEntry.Request = parseRequest(reqAndResp.req)
			harEntry.StartedDateTime = reqAndResp.start
			harEntry.Response = parseResponse(reqAndResp.resp)
			harEntry.Time = reqAndResp.end.Sub(reqAndResp.start).Nanoseconds() / 1e6
			fillIpAddress(reqAndResp.req, harEntry)
			proxy.HarLog.addEntry(*harEntry)
			proxy.entriesInProcess -= 1
		}()
	}
	log.Println("DONE PROCESSING ENTRIES")
}

func handleRequest(req *http.Request, harProxy *HarProxy) (*http.Request, *http.Response) {
	replaceHost(req, harProxy)
	return req, nil
}

func replaceHost(req *http.Request, harProxy *HarProxy) {
	for _, hostEntry := range harProxy.hostEntries {
		if req.URL.Host == hostEntry.Host {
			log.Println("Replacing ", hostEntry.Host, hostEntry.NewHost)
			req.URL.Host = hostEntry.NewHost
			return
		}
	}
}

func handleResponse(resp *http.Response, harEntry *HarEntry, harProxy *HarProxy) (newResp *http.Response, err error) {
	return resp, nil
}

func fillIpAddress(req *http.Request, harEntry *HarEntry) {
	host, _, err := net.SplitHostPort(req.URL.Host)
	if err != nil {
		host = req.URL.Host
	}
	if ip := net.ParseIP(host); ip != nil {
		harEntry.ServerIpAddress = string(ip)
	}

	if ipaddr, err := net.LookupIP(host); err == nil  {
		for _, ip := range ipaddr {
			if ip.To4() != nil {
				harEntry.ServerIpAddress = ip.String()
				return
			}
		}
	}
}

func (proxy *HarProxy) AddHostEntries(hostEntries []ProxyHosts) {
	entries := proxy.hostEntries
	m := len(entries)
	n := m + len(hostEntries)
	if n > cap(entries) { // if necessary, reallocate
		// allocate double what's needed, for future growth.
		newEntries := make([]ProxyHosts, (n+1)*2)
		copy(newEntries, entries)
		entries = newEntries
	}
	entries = entries[0:n]
	copy(entries[m:n], hostEntries)
	proxy.hostEntries = entries
}

func (proxy *HarProxy) Start() {
	l, err := net.Listen("tcp", ":" + strconv.Itoa(proxy.Port))
	if err != nil {
		log.Fatal("listen:", err)
	}
	proxy.StoppableListener = newStoppableListener(l)
	proxy.Port = GetPort(l)
	log.Printf("Starting harproxy server on port :%v", proxy.Port)
	go func() {
		http.Serve(proxy.StoppableListener, proxy.Proxy)
		log.Printf("Done serving proxy on port: %v", proxy.Port)

		// We notify twice to close both the mutex and the process entries routine
		close(proxy.entryChannel)
		proxy.isDone <- true

	}()
	log.Printf("Stared harproxy server on port :%v", proxy.Port)
}

func (proxy *HarProxy) Stop() {
	log.Printf("Stopping harproxy server on port :%v", proxy.Port)
	proxy.StoppableListener.Add(1)
	proxy.StoppableListener.Close()
	<-proxy.isDone
	proxy.StoppableListener.Done()
	proxy = nil
}

func (proxy *HarProxy) ClearEntries() {
	log.Printf("Clearing HAR for harproxy server on port :%v", proxy.Port)
	proxy.HarLog.Entries = nil
	proxy.HarLog.Entries = makeNewEntries()
}

func (proxy *HarProxy) NewHarReader() io.Reader {
	proxy.WaitForEntries()
	str, _ := json.Marshal(proxy.HarLog)
	return strings.NewReader(string(str))
}

func (proxy *HarProxy) WaitForEntries() {
	secs := 0
	for len(proxy.entryChannel) > 0 || proxy.entriesInProcess > 0 {
		log.Println("WAITING FOR ENTRIES")
		time.Sleep(1 * time.Second)
		secs++
		if secs > 10 {
			log.Printf("GIVING UP WAITING FOR ENTRIES AFTER %v SECONDS", secs)
		}
	}
}
//

// HarProxyServer

var portAndProxy map[int]*HarProxy = make(map[int]*HarProxy, 5000)

var portPathRegex *regexp.Regexp = regexp.MustCompile("/(\\d*)(/.*)?")

type ProxyServerPort struct {
	Port int   `json:"port"`
}

type ProxyServerErr struct {
	Error string	`json:"error"`
}

type ProxyServerMessage struct {
	Message string 		`json:"message"`
}

type ProxyHosts struct {
	Host 	string 		`json:"host"`
	NewHost string		`json:"NewHost"`
}

func addHostEntries(harProxy *HarProxy, r *http.Request, w http.ResponseWriter) {
	hostEntries := make([]ProxyHosts, 0, 10)
	err := json.NewDecoder(r.Body).Decode(&hostEntries)
	if err != nil {
		writeErrorMessage(w, http.StatusInternalServerError,  err.Error())
		return
	}

	harProxy.AddHostEntries(hostEntries)
	writeMessage(w, "Added hosts entries successfully")
}

func deleteHarProxy(port int, w http.ResponseWriter) {
	log.Printf("Deleting proxy on port :%v\n", port)
	harProxy := portAndProxy[port]
	harProxy.Stop()
	delete(portAndProxy, port)
	harProxy = nil
	writeMessage(w, fmt.Sprintf("Deleted proxy for port [%v] succesfully", port))
}

func getHarLog(harProxy *HarProxy, w http.ResponseWriter) {
	w.Header().Add("Content-Type", "application/json")
	harProxy.WaitForEntries()
	str, _ := json.Marshal(harProxy.HarLog)
	log.Println("Entry:", string(str))
	json.NewEncoder(w).Encode(harProxy.HarLog)
	harProxy.ClearEntries()

}

func createNewHarProxy(w http.ResponseWriter) {
	log.Printf("Got request to start new proxy\n")
	harProxy := NewHarProxy()
	harProxy.Start()
	port := GetPort(harProxy.StoppableListener.Listener)
	harProxy.Port = port

	portAndProxy[port] = harProxy

	w.Header().Add("Content-Type", "application/json")
	proxyServerPort := ProxyServerPort {
		Port : port,
	}
	json.NewEncoder(w).Encode(&proxyServerPort)
}

func getProxyForPath(path string, w http.ResponseWriter) (*HarProxy, string) {
	if portPathRegex.MatchString(path) {
		portStr := portPathRegex.FindStringSubmatch(path)[1]
		port, _ := strconv.Atoi(portStr)
		if portAndProxy[port] == nil {
			writeErrorMessage(w, http.StatusNotFound, fmt.Sprintf("No proxy for port [%v]", port))
			return nil, path
		}

		log.Printf("PORT:[%v]\n", port)
		return portAndProxy[port],  path[len("/" + portStr):]
	}

	return nil,path
}

func writeMessage(w http.ResponseWriter, msg string) {
	w.Header().Add("Content-type", "application/json")
	proxyMessage := ProxyServerMessage {
		Message : msg,
	}
	json.NewEncoder(w).Encode(&proxyMessage)
}

func writeErrorMessage(w http.ResponseWriter, httpStatus int,  msg string) {
	log.Printf("ERROR :[%v]", msg)
	w.WriteHeader(httpStatus)
	errorMessage := ProxyServerErr {
		Error : msg,
	}
	json.NewEncoder(w).Encode(&errorMessage)
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.URL.Path, "/proxy") {
		errHandler(w, r)
		return
	}
	path := r.URL.Path[len("/proxy"):]
	method := r.Method

	log.Printf("PATH:[%v]\n", r.URL.Path)
	log.Printf("FILTERED:[%v]\n", path)
	log.Printf("METHOD:[%v]\n", method)
	if path == "" && method == "POST" {
		log.Println("MATCH CREATE")
		createNewHarProxy(w)
		return
	}

	harProxy, path := getProxyForPath(path, w)
	switch {
	case harProxy == nil:
		return
	case strings.HasSuffix(path, "har") && method == "PUT":
		log.Println("MATCH PRINT")
		getHarLog(harProxy, w)
	case path == "" && method == "DELETE":
		log.Println("MATCH DELETE")
		deleteHarProxy(harProxy.Port, w)
	case strings.HasSuffix(path, "hosts") && method == "POST":
		log.Println("MATCH HOSTS")
		addHostEntries(harProxy, r, w)
	default:
		log.Printf("No such path: [%v]", path)
		writeErrorMessage(w, http.StatusNotFound, fmt.Sprintf("No such path [%s] with method %v" , path, method))
	}
}

func errHandler(w http.ResponseWriter, r *http.Request) {
	msg := fmt.Sprintf("No such path: [%v]", r.URL.Path)
	log.Println(msg)
	writeErrorMessage(w, http.StatusNotFound, msg)
}

func NewProxyServer(port int) {
	http.HandleFunc("/", errHandler)
	http.HandleFunc("/proxy", proxyHandler)
	http.HandleFunc("/proxy/", proxyHandler)

	log.Printf("Started HAR Proxy server on port :%v, Waiting for proxy start request\n", port)
	log.Fatal(http.ListenAndServe(":" + strconv.Itoa(port), nil))
}
