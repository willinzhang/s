package s

import (
	"fmt"
	"github.com/ssgo/log"
	"github.com/ssgo/standard"
	"github.com/ssgo/u"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ssgo/discover"
)

type proxyInfo struct {
	matcher  *regexp.Regexp
	fromPath string
	toApp    string
	toPath   string
}

var proxies = make(map[string]*proxyInfo, 0)
var regexProxies = make([]*proxyInfo, 0)
var proxyBy func(*http.Request) (*string, *string, *map[string]string)

// 跳转
func SetProxyBy(by func(request *http.Request) (toApp, toPath *string, headers *map[string]string)) {
	//forceDiscoverClient = true // 代理模式强制启动 Discover Client
	proxyBy = by
}

// 代理
func Proxy(path string, toApp, toPath string) {
	p := &proxyInfo{fromPath: path, toApp: toApp, toPath: toPath}
	if strings.Contains(path, "(") {
		matcher, err := regexp.Compile("^" + path + "$")
		if err != nil {
			logError(err.Error(), "expr", "^"+path+"$")
		} else {
			p.matcher = matcher
			regexProxies = append(regexProxies, p)
		}
	}
	if p.matcher == nil {
		proxies[path] = p
	}
}

// 查找 Proxy
func findProxy(request *http.Request) (*string, *string) {
	var requestPath string
	var queryString string
	pos := strings.LastIndex(request.RequestURI, "?")
	if pos != -1 {
		requestPath = request.RequestURI[0:pos]
		queryString = requestPath[pos:]
	} else {
		requestPath = request.RequestURI
	}
	pi := proxies[requestPath]
	if pi != nil {
		return &pi.toApp, &pi.toPath
	}
	if len(regexProxies) > 0 {
		for _, pi := range regexProxies {
			finds := pi.matcher.FindAllStringSubmatch(requestPath, 20)
			if len(finds) > 0 {
				toPath := pi.toPath
				for i, partValue := range finds[0] {
					toPath = strings.Replace(toPath, fmt.Sprintf("$%d", i), partValue, 10)
				}
				if queryString != "" {
					toPath += queryString
				}
				return &pi.toApp, &toPath
			}
		}
	}
	return nil, nil
}

// ProxyBy
func processProxy(request *http.Request, response *Response, logHeaders *map[string]string, startTime *time.Time, requestLogger *log.Logger) (finished bool) {
	proxyToApp, proxyToPath := findProxy(request)
	var proxyHeaders *map[string]string
	if proxyBy != nil && (proxyToApp == nil || proxyToPath == nil || *proxyToApp == "" || *proxyToPath == "") {
		proxyToApp, proxyToPath, proxyHeaders = proxyBy(request)
	}
	if proxyToApp == nil || proxyToPath == nil || *proxyToApp == "" || *proxyToPath == "" {
		return false
	}

	//if recordLogs {
	//	//log.Printf("PROXY	%s	%s	%s	%s	%s	%s", getRealIp(request), request.Host, request.Method, request.RequestURI, *proxyToApp, *proxyToPath)
	//}

	// 注册新的Call，并重启订阅
	if discover.Config.Calls == nil {
		discover.Config.Calls = make(map[string]string)
	}
	if discover.Config.Calls[*proxyToApp] == "" {
		//log.Printf("PROXY	add app	%s	for	%s	%s	%s", *proxyToApp, request.Host, request.Method, request.RequestURI)
		requestLogger.Info("add app on proxy", Map{
			"app":    proxyToApp,
			"ip":     getRealIp(request),
			"host":   request.Host,
			"method": request.Method,
			"uri":    request.RequestURI,
		})
		discover.Config.Calls[*proxyToApp] = u.String(Config.RewriteTimeout)
		discover.AddExternalApp(*proxyToApp, u.String(Config.RewriteTimeout))
		discover.Restart()
	}

	appConf := discover.Config.Calls[*proxyToApp]
	requestHeaders := make([]string, 0)
	if proxyHeaders != nil {
		for k, v := range *proxyHeaders {
			requestHeaders = append(requestHeaders, k, v)
		}
	}

	//if appConf.Headers != nil {
	//	for k, v := range appConf.Headers {
	//		requestHeaders = append(requestHeaders, k, v)
	//	}
	//}

	// 真实的用户IP，通过 X-Real-IP 续传
	requestHeaders = append(requestHeaders, standard.DiscoverHeaderClientIp, getRealIp(request))

	// 客户端IP列表，通过 X-Forwarded-For 接力续传
	requestHeaders = append(requestHeaders, standard.DiscoverHeaderForwardedFor, request.Header.Get(standard.DiscoverHeaderForwardedFor)+u.StringIf(request.Header.Get(standard.DiscoverHeaderForwardedFor) == "", "", ", ")+request.RemoteAddr[0:strings.IndexByte(request.RemoteAddr, ':')])

	// 客户唯一编号，通过 X-Client-ID 续传
	if request.Header.Get(standard.DiscoverHeaderClientId) != "" {
		requestHeaders = append(requestHeaders, standard.DiscoverHeaderClientId, request.Header.Get(standard.DiscoverHeaderClientId))
	}

	// 会话唯一编号，通过 X-Session-ID 续传
	if request.Header.Get(standard.DiscoverHeaderSessionId) != "" {
		requestHeaders = append(requestHeaders, standard.DiscoverHeaderSessionId, request.Header.Get(standard.DiscoverHeaderSessionId))
	}

	// 请求唯一编号，通过 X-Request-ID 续传
	requestId := request.Header.Get(standard.DiscoverHeaderRequestId)
	if requestId == "" {
		requestId = u.UniqueId()
		request.Header.Set(standard.DiscoverHeaderRequestId, requestId)
	}
	requestHeaders = append(requestHeaders, standard.DiscoverHeaderRequestId, requestId)

	// 真实用户请求的Host，通过 X-Host 续传
	host := request.Header.Get(standard.DiscoverHeaderHost)
	if host == "" {
		host = request.Host
		request.Header.Set(standard.DiscoverHeaderHost, host)
	}
	requestHeaders = append(requestHeaders, standard.DiscoverHeaderHost, host)

	outLen := 0
	//var outBytes []byte

	// 处理短连接 Proxy
	if request.Header.Get("Upgrade") == "websocket" {
		outLen = proxyWebsocketRequest(*proxyToApp, *proxyToPath, request, response, requestHeaders, appConf, requestLogger)
	} else {
		proxyWebRequest(*proxyToApp, *proxyToPath, request, response, requestHeaders, requestLogger)
		//outLen = proxyWebRequestReverse(*proxyToApp, *proxyToPath, request, response, requestHeaders, appConf.HttpVersion)
	}

	writeLog(requestLogger, "PROXY", nil, outLen, request, response, nil, logHeaders, startTime, 0, Map{
		"toApp":        proxyToApp,
		"toPath":       proxyToPath,
		"proxyHeaders": proxyHeaders,
	})
	return true
}

func proxyWebRequest(app, path string, request *http.Request, response *Response, requestHeaders []string, requestLogger *log.Logger) {
	//var bodyBytes []byte = nil
	//if request.Body != nil {
	//	bodyBytes, _ = ioutil.ReadAll(request.Body)
	//	request.Body.Close()
	//}
	caller := &discover.Caller{Request: request, NoBody: true}
	r := caller.Do(request.Method, app, path, request.Body, requestHeaders...)

	//var statusCode int
	//var outBytes []byte
	if r.Error == nil && r.Response != nil {
		//statusCode = r.Response.StatusCode
		//outBytes = r.Bytes()
		for k, v := range r.Response.Header {
			response.Header().Set(k, v[0])
		}
		response.WriteHeader(r.Response.StatusCode)
		outLen, err := io.Copy(response.writer, r.Response.Body)
		if err != nil {
			response.WriteHeader(500)
			n, err := response.Write([]byte(err.Error()))
			if err != nil {
				requestLogger.Error(err.Error(), "wrote", n)
			}
			response.outLen = int(len(err.Error()))
			//statusCode = 500
			//outBytes = []byte(r.Error.Error())
		} else {
			response.outLen = int(outLen)
		}
	} else {
		//statusCode = 500
		//outBytes = []byte(r.Error.Error())
		response.WriteHeader(500)
		n, err := response.Write([]byte(r.Error.Error()))
		if err != nil {
			requestLogger.Error(err.Error(), "wrote", n)
		}
		response.outLen = int(len(r.Error.Error()))
	}
}

var updater = websocket.Upgrader{}

func proxyWebsocketRequest(app, path string, request *http.Request, response *Response, requestHeaders []string, appConf string, requestLogger *log.Logger) int {
	srcConn, err := updater.Upgrade(response.writer, request, nil)
	if err != nil {
		requestLogger.Error(err.Error(), Map{
			"app":    app,
			"path":   path,
			"ip":     getRealIp(request),
			"method": request.Method,
			"host":   request.Host,
			"uri":    request.RequestURI,
		})
		//log.Printf("PROXY	Upgrade	%s", err.Error())
		return 0
	}
	defer func() {
		_ = srcConn.Close()
	}()

	appClient := discover.AppClient{}
	var node *discover.NodeInfo
	for {
		node = appClient.NextWithNode(app, "", request)
		if node == nil {
			break
		}

		// 请求节点
		node.UsedTimes++

		scheme := "ws"
		//if appConf.WithSSL {
		//	scheme += "s"
		//}
		parsedUrl, err := url.Parse(fmt.Sprintf("%s://%s%s", scheme, node.Addr, path))
		if err != nil {
			requestLogger.Error(err.Error(), Map{
				"app":    app,
				"path":   path,
				"ip":     getRealIp(request),
				"method": request.Method,
				"host":   request.Host,
				"uri":    request.RequestURI,
				"url":    parsedUrl.String(),
			})
			//log.Printf("PROXY	parsing websocket address	%s", err.Error())
			return 0
		}

		sendHeader := http.Header{}
		for k, vv := range request.Header {
			if k != "Connection" && k != "Upgrade" && !strings.Contains(k, "Sec-Websocket-") {
				for _, v := range vv {
					sendHeader.Add(k, v)
				}
			}
		}
		for i := 1; i < len(requestHeaders); i += 2 {
			sendHeader.Set(requestHeaders[i-1], requestHeaders[i])
		}

		//if httpVersion != 1 {
		//	rp.Transport = &http2.Transport{
		//		AllowHTTP: true,
		//		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
		//			return net.Dial(network, addr)
		//		},
		//	}
		//}

		dialer := websocket.Dialer{}
		dstConn, dstResponse, err := dialer.Dial(parsedUrl.String(), sendHeader)
		if err != nil {
			requestLogger.Error(err.Error(), Map{
				"app":    app,
				"path":   path,
				"ip":     getRealIp(request),
				"method": request.Method,
				"host":   request.Host,
				"uri":    request.RequestURI,
				"url":    parsedUrl.String(),
			})
			//log.Printf("PROXY	opening client websocket connection	%s", err.Error())
			continue
		}
		if dstResponse.StatusCode == 502 || dstResponse.StatusCode == 503 || dstResponse.StatusCode == 504 {
			_ = dstConn.Close()
			continue
		}

		waits := make(chan bool, 2)
		totalOutLen := 0
		go func() {
			for {
				mt, message, err := dstConn.ReadMessage()
				if err != nil {
					if !strings.Contains(err.Error(), "websocket: close ") {
						requestLogger.Error(err.Error(), Map{
							"app":    app,
							"path":   path,
							"ip":     getRealIp(request),
							"method": request.Method,
							"host":   request.Host,
							"uri":    request.RequestURI,
							"url":    parsedUrl.String(),
						})
						//log.Print("PROXY	WS Error	reading message from the client websocket	", err)
					}
					break
				}
				totalOutLen += len(message)
				err = srcConn.WriteMessage(mt, message)
				if err != nil {
					requestLogger.Error(err.Error(), Map{
						"app":    app,
						"path":   path,
						"ip":     getRealIp(request),
						"method": request.Method,
						"host":   request.Host,
						"uri":    request.RequestURI,
						"url":    parsedUrl.String(),
					})
					//log.Print("PROXY	WS Error	writing message to the server websocket	", err)
					break
				}
			}
			waits <- true
			_ = dstConn.Close()
		}()

		go func() {
			for {
				mt, message, err := srcConn.ReadMessage()
				if err != nil {
					if !strings.Contains(err.Error(), "websocket: close ") {
						requestLogger.Error(err.Error(), Map{
							"app":    app,
							"path":   path,
							"ip":     getRealIp(request),
							"method": request.Method,
							"host":   request.Host,
							"uri":    request.RequestURI,
							"url":    parsedUrl.String(),
						})
						//log.Print("PROXY	WS Error	reading message from the server websocket	", err)
					}
					break
				}
				err = dstConn.WriteMessage(mt, message)
				if err != nil {
					requestLogger.Error(err.Error(), Map{
						"app":    app,
						"path":   path,
						"ip":     getRealIp(request),
						"method": request.Method,
						"host":   request.Host,
						"uri":    request.RequestURI,
						"url":    parsedUrl.String(),
					})
					//log.Print("PROXY	WS Error	writing message to the server websocket	", err)
					break
				}
			}
			waits <- true
			_ = srcConn.Close()
		}()

		<-waits
		return totalOutLen
	}

	return 0
}

//func proxyWebRequestReverse(app, path string, request *http.Request, response *Response, requestHeaders []string, httpVersion int) int {
//	appClient := discover.AppClient{}
//	var node *discover.NodeInfo
//	for {
//		node = appClient.NextWithNode(app, "", request)
//		if node == nil {
//			break
//		}
//
//		// 请求节点
//		node.UsedTimes++
//
//		rp := &httputil.ReverseProxy{Director: func(req *http.Request) {
//			req.URL.Scheme = u.StringIf(request.URL.Scheme == "", "http", request.URL.Scheme)
//			if request.TLS != nil {
//				req.URL.Scheme += "s"
//			}
//			req.URL.Host = node.Addr
//			req.URL.Path = path
//			for k, vv := range request.Header {
//				for _, v := range vv {
//					req.Header.Add(k, v)
//				}
//			}
//			for i := 1; i < len(requestHeaders); i += 2 {
//				if requestHeaders[i-1] == "Host" {
//					req.Host = requestHeaders[i]
//				} else {
//					req.Header.Set(requestHeaders[i-1], requestHeaders[i])
//				}
//			}
//		}}
//		if httpVersion != 1 {
//			rp.Transport = &http2.Transport{
//				AllowHTTP: true,
//				DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
//					return net.Dial(network, addr)
//				},
//			}
//		}
//		response.ProxyHeader = &http.Header{}
//		rp.ServeHTTP(response, request)
//		if response.status != 502 && response.status != 503 && response.status != 504 {
//			break
//		}
//	}
//
//	return response.outLen
//}
