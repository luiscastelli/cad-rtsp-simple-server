package core

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	gopath "path"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luiscastelli/cad-rtsp-simple-server/internal/conf"
	"github.com/luiscastelli/cad-rtsp-simple-server/internal/hls"
	"github.com/luiscastelli/cad-rtsp-simple-server/internal/logger"
)

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

type hlsServerAPIMuxersListItem struct {
	Created     time.Time `json:"created"`
	LastRequest string    `json:"lastRequest"`
}

type hlsServerAPIMuxersListData struct {
	Items map[string]hlsServerAPIMuxersListItem `json:"items"`
}

type hlsServerAPIMuxersListRes struct {
	data   *hlsServerAPIMuxersListData
	muxers map[string]*hlsMuxer
	err    error
}

type hlsServerAPIMuxersListReq struct {
	res chan hlsServerAPIMuxersListRes
}

type hlsServerAPIMuxersListSubReq struct {
	data *hlsServerAPIMuxersListData
	res  chan struct{}
}

type hlsServerParent interface {
	Log(logger.Level, string, ...interface{})
}

type hlsServer struct {
	externalAuthenticationURL string
	hlsAlwaysRemux            bool
	hlsVariant                conf.HLSVariant
	hlsSegmentCount           int
	hlsSegmentDuration        conf.StringDuration
	hlsPartDuration           conf.StringDuration
	hlsSegmentMaxSize         conf.StringSize
	hlsAllowOrigin            string
	hlsTrustedProxies         conf.IPsOrCIDRs
	readBufferCount           int
	pathManager               *pathManager
	metrics                   *metrics
	parent                    hlsServerParent

	ctx       context.Context
	ctxCancel func()
	wg        sync.WaitGroup
	ln        net.Listener
	tlsConfig *tls.Config
	muxers    map[string]*hlsMuxer

	// in
	chPathSourceReady    chan *path
	chPathSourceNotReady chan *path
	request              chan *hlsMuxerRequest
	chMuxerClose         chan *hlsMuxer
	chAPIMuxerList       chan hlsServerAPIMuxersListReq
}

func newHLSServer(
	parentCtx context.Context,
	address string,
	externalAuthenticationURL string,
	hlsAlwaysRemux bool,
	hlsVariant conf.HLSVariant,
	hlsSegmentCount int,
	hlsSegmentDuration conf.StringDuration,
	hlsPartDuration conf.StringDuration,
	hlsSegmentMaxSize conf.StringSize,
	hlsAllowOrigin string,
	hlsEncryption bool,
	hlsServerKey string,
	hlsServerCert string,
	hlsTrustedProxies conf.IPsOrCIDRs,
	readBufferCount int,
	pathManager *pathManager,
	metrics *metrics,
	parent hlsServerParent,
) (*hlsServer, error) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	var tlsConfig *tls.Config
	if hlsEncryption {
		crt, err := tls.LoadX509KeyPair(hlsServerCert, hlsServerKey)
		if err != nil {
			ln.Close()
			return nil, err
		}

		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{crt},
		}
	}

	ctx, ctxCancel := context.WithCancel(parentCtx)

	s := &hlsServer{
		externalAuthenticationURL: externalAuthenticationURL,
		hlsAlwaysRemux:            hlsAlwaysRemux,
		hlsVariant:                hlsVariant,
		hlsSegmentCount:           hlsSegmentCount,
		hlsSegmentDuration:        hlsSegmentDuration,
		hlsPartDuration:           hlsPartDuration,
		hlsSegmentMaxSize:         hlsSegmentMaxSize,
		hlsAllowOrigin:            hlsAllowOrigin,
		hlsTrustedProxies:         hlsTrustedProxies,
		readBufferCount:           readBufferCount,
		pathManager:               pathManager,
		parent:                    parent,
		metrics:                   metrics,
		ctx:                       ctx,
		ctxCancel:                 ctxCancel,
		ln:                        ln,
		tlsConfig:                 tlsConfig,
		muxers:                    make(map[string]*hlsMuxer),
		chPathSourceReady:         make(chan *path),
		chPathSourceNotReady:      make(chan *path),
		request:                   make(chan *hlsMuxerRequest),
		chMuxerClose:              make(chan *hlsMuxer),
		chAPIMuxerList:            make(chan hlsServerAPIMuxersListReq),
	}

	s.log(logger.Info, "listener opened on "+address)

	s.pathManager.hlsServerSet(s)

	if s.metrics != nil {
		s.metrics.hlsServerSet(s)
	}

	s.wg.Add(1)
	go s.run()

	return s, nil
}

// Log is the main logging function.
func (s *hlsServer) log(level logger.Level, format string, args ...interface{}) {
	s.parent.Log(level, "[HLS] "+format, append([]interface{}{}, args...)...)
}

func (s *hlsServer) close() {
	s.log(logger.Info, "listener is closing")
	s.ctxCancel()
	s.wg.Wait()
}

func (s *hlsServer) run() {
	defer s.wg.Done()

	router := gin.New()
	router.NoRoute(s.onRequest)

	tmp := make([]string, len(s.hlsTrustedProxies))
	for i, entry := range s.hlsTrustedProxies {
		tmp[i] = entry.String()
	}
	router.SetTrustedProxies(tmp)

	hs := &http.Server{
		Handler:   router,
		TLSConfig: s.tlsConfig,
		ErrorLog:  log.New(&nilWriter{}, "", 0),
	}

	if s.tlsConfig != nil {
		go hs.ServeTLS(s.ln, "", "")
	} else {
		go hs.Serve(s.ln)
	}

outer:
	for {
		select {
		case pa := <-s.chPathSourceReady:
			if s.hlsAlwaysRemux {
				s.findOrCreateMuxer(pa.Name(), "", nil)
			}

		case pa := <-s.chPathSourceNotReady:
			if s.hlsAlwaysRemux {
				c, ok := s.muxers[pa.Name()]
				if ok {
					c.close()
					delete(s.muxers, pa.Name())
				}
			}

		case req := <-s.request:
			s.findOrCreateMuxer(req.dir, req.ctx.ClientIP(), req)

		case c := <-s.chMuxerClose:
			if c2, ok := s.muxers[c.PathName()]; !ok || c2 != c {
				continue
			}
			delete(s.muxers, c.PathName())

			if s.hlsAlwaysRemux && c.remoteAddr == "" {
				s.findOrCreateMuxer(c.PathName(), "", nil)
			}

		case req := <-s.chAPIMuxerList:
			muxers := make(map[string]*hlsMuxer)

			for name, m := range s.muxers {
				muxers[name] = m
			}

			req.res <- hlsServerAPIMuxersListRes{
				muxers: muxers,
			}

		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()

	hs.Shutdown(context.Background())

	s.pathManager.hlsServerSet(nil)

	if s.metrics != nil {
		s.metrics.hlsServerSet(nil)
	}
}

func (s *hlsServer) onRequest(ctx *gin.Context) {
	s.log(logger.Debug, "[conn %v] %s %s", ctx.ClientIP(), ctx.Request.Method, ctx.Request.URL.Path)

	byts, _ := httputil.DumpRequest(ctx.Request, true)
	s.log(logger.Debug, "[conn %v] [c->s] %s", ctx.ClientIP(), string(byts))

	logw := &httpLogWriter{ResponseWriter: ctx.Writer}
	ctx.Writer = logw

	ctx.Writer.Header().Set("Server", "rtsp-simple-server")
	ctx.Writer.Header().Set("Access-Control-Allow-Origin", s.hlsAllowOrigin)
	ctx.Writer.Header().Set("Access-Control-Allow-Credentials", "true")

	switch ctx.Request.Method {
	case http.MethodGet:

	case http.MethodOptions:
		ctx.Writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		ctx.Writer.Header().Set("Access-Control-Allow-Headers", ctx.Request.Header.Get("Access-Control-Request-Headers"))
		ctx.Writer.WriteHeader(http.StatusOK)
		return

	default:
		ctx.Writer.WriteHeader(http.StatusNotFound)
		return
	}

	// remove leading prefix
	pa := ctx.Request.URL.Path[1:]

	switch pa {
	case "", "favicon.ico":
		ctx.Writer.WriteHeader(http.StatusNotFound)
		return
	}

	dir, fname := func() (string, string) {
		if strings.HasSuffix(pa, ".m3u8") ||
			strings.HasSuffix(pa, ".ts") ||
			strings.HasSuffix(pa, ".mp4") {
			return gopath.Dir(pa), gopath.Base(pa)
		}
		return pa, ""
	}()

	if fname == "" && !strings.HasSuffix(dir, "/") {
		ctx.Writer.Header().Set("Location", "/"+dir+"/")
		ctx.Writer.WriteHeader(http.StatusMovedPermanently)
		return
	}

	dir = strings.TrimSuffix(dir, "/")

	cres := make(chan func() *hls.MuxerFileResponse)
	hreq := &hlsMuxerRequest{
		dir:  dir,
		file: fname,
		ctx:  ctx,
		res:  cres,
	}

	select {
	case s.request <- hreq:
		cb := <-cres

		res := cb()

		for k, v := range res.Header {
			ctx.Writer.Header().Set(k, v)
		}

		ctx.Writer.WriteHeader(res.Status)

		if res.Body != nil {
			io.Copy(ctx.Writer, res.Body)
		}

	case <-s.ctx.Done():
	}

	s.log(logger.Debug, "[conn %v] [s->c] %s", ctx.ClientIP(), logw.dump())
}

func (s *hlsServer) findOrCreateMuxer(pathName string, remoteAddr string, req *hlsMuxerRequest) *hlsMuxer {
	r, ok := s.muxers[pathName]
	if !ok {
		r = newHLSMuxer(
			s.ctx,
			pathName,
			remoteAddr,
			s.externalAuthenticationURL,
			s.hlsVariant,
			s.hlsSegmentCount,
			s.hlsSegmentDuration,
			s.hlsPartDuration,
			s.hlsSegmentMaxSize,
			s.readBufferCount,
			req,
			&s.wg,
			pathName,
			s.pathManager,
			s)
		s.muxers[pathName] = r
	} else if req != nil {
		r.request(req)
	}
	return r
}

// muxerClose is called by hlsMuxer.
func (s *hlsServer) muxerClose(c *hlsMuxer) {
	select {
	case s.chMuxerClose <- c:
	case <-s.ctx.Done():
	}
}

// pathSourceReady is called by pathManager.
func (s *hlsServer) pathSourceReady(pa *path) {
	select {
	case s.chPathSourceReady <- pa:
	case <-s.ctx.Done():
	}
}

// pathSourceNotReady is called by pathManager.
func (s *hlsServer) pathSourceNotReady(pa *path) {
	select {
	case s.chPathSourceNotReady <- pa:
	case <-s.ctx.Done():
	}
}

// apiHLSMuxersList is called by api.
func (s *hlsServer) apiHLSMuxersList(req hlsServerAPIMuxersListReq) hlsServerAPIMuxersListRes {
	req.res = make(chan hlsServerAPIMuxersListRes)
	select {
	case s.chAPIMuxerList <- req:
		res := <-req.res

		res.data = &hlsServerAPIMuxersListData{
			Items: make(map[string]hlsServerAPIMuxersListItem),
		}

		for _, pa := range res.muxers {
			pa.apiHLSMuxersList(hlsServerAPIMuxersListSubReq{data: res.data})
		}

		return res

	case <-s.ctx.Done():
		return hlsServerAPIMuxersListRes{err: fmt.Errorf("terminated")}
	}
}
