package s

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"github.com/ssgo/log"
	"github.com/ssgo/standard"
	"github.com/ssgo/u"
	"golang.org/x/net/websocket"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type Response struct {
	writer      http.ResponseWriter
	status      int
	outLen      int
	ProxyHeader *http.Header
}

func (response *Response) Header() http.Header {
	if response.ProxyHeader != nil {
		return *response.ProxyHeader
	}
	return response.writer.Header()
}
func (response *Response) Write(bytes []byte) (int, error) {
	response.outLen += len(bytes)
	if response.ProxyHeader != nil {
		response.copyProxyHeader()
	}
	return response.writer.Write(bytes)
}
func (response *Response) WriteHeader(code int) {
	response.status = code
	if response.ProxyHeader != nil && (response.status == 502 || response.status == 503 || response.status == 504) {
		return
	}
	response.writer.WriteHeader(code)
	if response.ProxyHeader != nil {
		response.copyProxyHeader()
	}
}

func (response *Response) copyProxyHeader() {
	src := *response.ProxyHeader
	dst := response.writer.Header()
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	response.ProxyHeader = nil
}

//func (response *Response) Hijacker() (net.Conn, *bufio.ReadWriter, error) {
//	h, ok := response.writer.(http.Hijacker)
//	if !ok {
//		return nil, nil, errors.New("bad Hijacker")
//	}
//	return h.Hijack()
//}

type routeHandler struct {
	webRequestingNum int64
	wsConns          map[string]*websocket.Conn
	// TODO 记录正在处理的请求数量，连接中的WS数量，在关闭服务时能优雅的结束
}

func (rh *routeHandler) Stop() {
	for _, conn := range rh.wsConns {
		conn.Close()
	}
}

func (rh *routeHandler) Wait() {
	for i := 0; i < 25; i++ {
		if rh.webRequestingNum == 0 && len(rh.wsConns) == 0 {
			break
		}
		time.Sleep(time.Millisecond * 200)
	}
}

func (rh *routeHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var myResponse *Response = &Response{writer: writer, status: 200}
	var response http.ResponseWriter = myResponse
	startTime := time.Now()

	// 产生 X-Request-ID
	if request.Header.Get(standard.DiscoverHeaderRequestId) == "" {
		request.Header.Set(standard.DiscoverHeaderRequestId, u.UniqueId())
		if !conf.AcceptXRealIpWithoutRequestId {
			// 在没有 X-Request-ID 的情况下忽略 X-Real-IP
			if request.Header.Get(standard.DiscoverHeaderClientIp) != "" {
				request.Header.Del(standard.DiscoverHeaderClientIp)
			}
		}
	}

	// 真实的用户IP，通过 X-Real-IP 续传
	request.Header.Set(standard.DiscoverHeaderClientIp, getRealIp(request))

	// 请求唯一编号，通过 X-Request-ID 续传
	requestId := request.Header.Get(standard.DiscoverHeaderRequestId)
	if requestId == "" {
		requestId = u.UniqueId()
		request.Header.Set(standard.DiscoverHeaderRequestId, requestId)
	}

	// 真实用户请求的Host，通过 X-Host 续传
	host := request.Header.Get(standard.DiscoverHeaderHost)
	if host == "" {
		host = request.Host
		request.Header.Set(standard.DiscoverHeaderHost, host)
	}

	// 真实用户请求的Scheme，通过 X-Scheme 续传
	scheme := request.Header.Get(standard.DiscoverHeaderScheme)
	if scheme == "" {
		scheme = u.StringIf(request.TLS == nil, "http", "https")
		request.Header.Set(standard.DiscoverHeaderScheme, scheme)
	}

	// SessionId
	if sessionKey != "" {
		if request.Header.Get(sessionKey) == "" {
			var newSessionid string
			if sessionCreator == nil {
				newSessionid = u.UniqueId()
			} else {
				newSessionid = sessionCreator()
			}
			request.Header.Set(sessionKey, newSessionid)
			response.Header().Set(sessionKey, newSessionid)
		}
		// 为了在服务间调用时续传 SessionId
		request.Header.Set(standard.DiscoverHeaderSessionId, request.Header.Get(sessionKey))
	}

	if clientKey != "" {
		// 为了在服务间调用时续传 ClientId
		request.Header.Set(standard.DiscoverHeaderClientId, request.Header.Get(clientKey))
	}

	// Headers，未来可以优化日志记录，最近访问过的头部信息可省略
	logHeaders := make(map[string]string)
	for k, v := range request.Header {
		if noLogHeaders[strings.ToLower(k)] {
			continue
		}
		if len(v) > 1 {
			logHeaders[k] = strings.Join(v, ", ")
		} else {
			logHeaders[k] = v[0]
		}
	}

	// 处理 Rewrite，如果是外部转发，直接结束请求
	finished := processRewrite(request, myResponse, &logHeaders, &startTime)
	if finished {
		return
	}

	// 处理 ProxyBy
	finished = processProxy(request, myResponse, &logHeaders, &startTime)
	if finished {
		return
	}

	var requestPath string
	pos := strings.LastIndex(request.RequestURI, "?")
	if pos != -1 {
		requestPath = request.RequestURI[0:pos]
	} else {
		requestPath = request.RequestURI
	}

	// 处理静态文件
	if processStatic(requestPath, request, myResponse, &logHeaders, &startTime) {
		return
	}

	args := make(map[string]interface{})

	// 先看缓存中是否有 Service
	var s *webServiceType
	var ws *websocketServiceType
	s = webServices[request.Method+requestPath]
	if s == nil {
		s = webServices[requestPath]
		if s == nil {
			ws = websocketServices[requestPath]
		}
	}

	// 未匹配到缓存，尝试匹配新的 Service
	if s == nil && ws == nil {
		for _, tmpS := range regexWebServices {
			if tmpS.method != "" && tmpS.method != request.Method {
				continue
			}
			finds := tmpS.pathMatcher.FindAllStringSubmatch(requestPath, 20)
			if len(finds) > 0 {
				foundArgs := finds[0]
				for i := 1; i < len(foundArgs); i++ {
					//log.Println("  >>>>", tmpS.pathArgs[i-1], foundArgs[i])
					args[tmpS.pathArgs[i-1]] = foundArgs[i]
					s = tmpS
				}
				break
			}
		}
	}

	// 未匹配到缓存和Service，尝试匹配新的WebsocketService
	if s == nil && ws == nil {
		for _, tmpS := range regexWebsocketServices {
			finds := tmpS.pathMatcher.FindAllStringSubmatch(requestPath, 20)
			if len(finds) > 0 {
				foundArgs := finds[0]
				for i := 1; i < len(foundArgs); i++ {
					args[tmpS.pathArgs[i-1]] = foundArgs[i]
					ws = tmpS
				}
				break
			}
		}
	}

	// 全都未匹配，输出404
	if s == nil && ws == nil {
		response.WriteHeader(404)
		if requestPath != "/favicon.ico" {
			writeLog("FAIL", nil, 0, request, myResponse, &args, &logHeaders, &startTime, 0, nil)
		}
		return
	}
	//判定是rewrite
	// rewrite问号后的参数不能被request.Form解析 解析问号后的参数
	if strings.Index(request.RequestURI, request.URL.Path) == -1 && strings.LastIndex(request.RequestURI, "?") != -1 {
		requestUrl, reqErr := url.Parse(request.RequestURI)
		if reqErr == nil {
			queryStringArr, reqErr := url.ParseQuery(requestUrl.RawQuery)
			if reqErr == nil && len(queryStringArr) > 0 {
				for paramName, paramValue := range queryStringArr {
					if len(paramValue) < 1 {
						continue
					}
					if len(paramValue) > 1 {
						args[paramName] = paramValue
					} else {
						args[paramName] = paramValue[0]
					}

				}
			}
		}
	}
	// GET POST
	request.ParseForm()
	reqForm := request.Form
	for k, v := range reqForm {
		if len(v) > 1 {
			args[k] = v
		} else {
			args[k] = v[0]
		}
	}

	// POST
	if request.Body != nil {
		contentType := request.Header.Get("Content-Type")
		if contentType == "application/json" {
			bodyBytes, _ := ioutil.ReadAll(request.Body)
			_ = request.Body.Close()
			if len(bodyBytes) > 0 {
				var err error
				if bodyBytes[0] == 123 {
					err = json.Unmarshal(bodyBytes, &args)
				} else {
					arg := new(interface{})
					err = json.Unmarshal(bodyBytes, arg)
					args["request"] = arg
				}
				if err != nil {
					response.WriteHeader(400)
					writeLog("FAIL", nil, 0, request, myResponse, &args, &logHeaders, &startTime, 0, nil)
					return
				}
			}
		} else if contentType == "application/x-www-form-urlencoded" {
			bodyBytes, _ := ioutil.ReadAll(request.Body)
			_ = request.Body.Close()
			argsBody, err := url.ParseQuery(string(bodyBytes))
			if err == nil && len(argsBody) > 0 {
				for aKey, aBody := range argsBody {
					if len(aBody) > 1 {
						args[aKey] = aBody
					} else {
						args[aKey] = aBody[0]
					}
				}
			}
		}
	}

	// 身份认证
	var authLevel uint = 0
	if ws != nil {
		authLevel = ws.authLevel
	} else if s != nil {
		authLevel = s.authLevel
	}

	defer func() {
		if err := recover(); err != nil {
			var out interface{}
			if errorHandle != nil {
				out = errorHandle(err, request, &response)
			} else {
				response.WriteHeader(ResponseCodePanicError)
				out = ""
			}
			writeLog("PANIC", out, myResponse.outLen, request, myResponse, &args, &logHeaders, &startTime, authLevel, Map{
				"error": err,
			})
		}
	}()

	// 前置过滤器
	var result interface{} = nil
	for _, filter := range inFilters {
		result = filter(&args, request, &response)
		if result != nil {
			break
		}
	}
	if authLevel > 0 {
		if webAuthChecker == nil {
			SetAuthChecker(func(authLevel uint, url *string, in *map[string]interface{}, request *http.Request) bool {
				settedAuthLevel := conf.AccessTokens[request.Header.Get("Access-Token")]
				//log.Println(" ***** ", request.Header.Get("Access-Token"), conf.AccessTokens[request.Header.Get("Access-Token")], authLevel)
				return settedAuthLevel != nil && *settedAuthLevel >= authLevel
			})
		}
		if webAuthChecker(authLevel, &request.RequestURI, &args, request) == false {
			//usedTime := float32(time.Now().UnixNano()-startTime.UnixNano()) / 1e6
			//byteArgs, _ := json.Marshal(args)
			//byteHeaders, _ := json.Marshal(logHeaders)
			//log.Printf("REJECT	%s	%s	%s	%s	%.6f	%s	%s	%d	%s", request.RemoteAddr, request.Host, request.Method, request.RequestURI, usedTime, string(byteArgs), string(byteHeaders), authLevel, request.Proto)
			response.WriteHeader(403)
			writeLog("REJECT", result, 0, request, myResponse, &args, &logHeaders, &startTime, authLevel, nil)
			return
		}
	}

	// 处理 Proxy
	//var logName string
	//var statusCode int
	//if proxyToApp != nil {
	//	caller := &Caller{request: request}
	//	r := caller.Do(request.Method, *proxyToApp, *proxyToPath, args, "Host", request.Host)
	//	result = r.Bytes()
	//	statusCode = 500
	//	if r.Error == nil && r.Response != nil {
	//		statusCode = r.Response.StatusCode
	//	}
	//	logName = "PROXY"
	//} else {
	// 处理 Websocket
	if ws != nil && result == nil {
		doWebsocketService(ws, request, myResponse, authLevel, &args, &logHeaders, &startTime)
	} else if s != nil || result != nil {
		result = doWebService(s, request, &response, &args, &logHeaders, result, &startTime)
		//logName = "ACCESS"
		//statusCode = 200
	}
	//}

	if ws == nil {
		// 后置过滤器
		for _, filter := range outFilters {
			newResult, done := filter(&args, request, &response, result)
			if newResult != nil {
				result = newResult
			}
			if done {
				break
			}
		}
		// 返回结果
		outType := reflect.TypeOf(result)
		if outType == nil {
			return
		}
		for outType.Kind() == reflect.Ptr {
			outType = outType.Elem()
		}
		var outBytes []byte
		if outType.Kind() != reflect.String && (outType.Kind() != reflect.Slice || outType.Elem().Kind() != reflect.Uint8) {
			outBytes = makeBytesResult(result)
		} else if outType.Kind() == reflect.String {
			outBytes = []byte(result.(string))
		} else {
			outBytes = result.([]byte)
		}

		isZipOuted := false
		if conf.Compress && len(outBytes) >= conf.CompressMinSize && len(outBytes) <= conf.CompressMaxSize && strings.Contains(request.Header.Get("Accept-Encoding"), "gzip") {
			zipWriter, err := gzip.NewWriterLevel(response, 1)
			if err == nil {
				response.Header().Set("Content-Encoding", "gzip")
				zipWriter.Write(outBytes)
				zipWriter.Close()
				isZipOuted = true
			}
		}

		if !isZipOuted {
			response.Write(outBytes)
		}

		// 记录访问日志
		if recordLogs {
			outLen := 0
			if outBytes != nil {
				outLen = len(outBytes)
			}
			//if logName != "ACCESS" {
			//	if outBytes != nil {
			//		outLen = len(outBytes)
			//		outBytes = nil
			//	}
			//}

			// /__CHECK__ 不记录日志
			if requestPath != "/__CHECK__" {
				writeLog("ACCESS", result, outLen, request, myResponse, &args, &logHeaders, &startTime, authLevel, nil)
			}
		}
	}

	if sessionObjects[request] != nil {
		delete(sessionObjects, request)
	}
}

func requireEncryptField(k string) bool {
	return encryptLogFields[strings.ToLower(strings.Replace(k, "-", "", 3))]
}

func encryptField(value interface{}) string {
	v := u.String(value)
	if len(v) > 12 {
		return v[0:3] + "***" + v[len(v)-3:]
	} else if len(v) > 8 {
		return v[0:2] + "***" + v[len(v)-2:]
	} else if len(v) > 4 {
		return v[0:1] + "***" + v[len(v)-1:]
	} else {
		return "***"
	}
}

func writeLog(logName string, result interface{}, outLen int, request *http.Request, response *Response, args *map[string]interface{}, headers *map[string]string, startTime *time.Time, authLevel uint, extraInfo Map) {
	if conf.NoLogGets && request.Method == "GET" {
		return
	}
	usedTime := float32(time.Now().UnixNano()-startTime.UnixNano()) / 1e6
	//var byteArgs []byte
	//if args != nil {
	//	byteArgs, _ = json.Marshal(*args)
	//}
	//var byteHeaders []byte
	if headers != nil {
		for k, v := range *headers {
			if requireEncryptField(k) {
				(*headers)[k] = encryptField(v)
			}
		}
		//byteHeaders, _ = json.Marshal(*headers)
	}

	outHeaders := make(map[string]string)
	for k, v := range (*response).Header() {
		if outLen == 0 && k == "Content-Length" {
			outLen, _ = strconv.Atoi(v[0])
		}
		if noLogHeaders[strings.ToLower(k)] {
			continue
		}
		if len(v) > 1 {
			outHeaders[k] = strings.Join(v, ", ")
		} else {
			outHeaders[k] = v[0]
		}

		if requireEncryptField(k) {
			outHeaders[k] = encryptField(outHeaders[k])
		}
	}

	//var args2 interface{}
	//if conf.NoLogInputFields == false {
	//	for k, v := range *args {
	//		if requireEncryptField(k) {
	//			(*args)[k] = encryptField(v)
	//		}
	//
	//		t := reflect.TypeOf(v)
	//		for t.Kind() == reflect.Ptr {
	//			t = t.Elem()
	//		}
	//		if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
	//			if conf.LogInputArrayNum == 0 {
	//				(*args)[k] = fmt.Sprintln(reflect.ValueOf(v).Len(), t.String())
	//			}
	//		}
	//	}
	//	args2 = makeLogableData(reflect.ValueOf(args), conf.LogInputArrayNum).Interface()
	//}

	//ov := reflect.ValueOf(result)
	//ot := ov.Type()
	//for ot.Kind() == reflect.Ptr {
	//	ot = ot.Elem()
	//	ov = ov.Elem()
	//}
	//if len(logOutputFields) > 0 {
	//	if ot.Kind() == reflect.Map {
	//		if requireEncryptField(k) {
	//			(*args)[k] = encryptField(v)
	//		}
	//
	//		t := reflect.TypeOf(v)
	//		for t.Kind() == reflect.Ptr {
	//			t = t.Elem()
	//		}
	//		if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
	//			if conf.LogInputArrayNum == 0 {
	//				(*args)[k] = fmt.Sprintln(reflect.ValueOf(v).Len(), t.String())
	//			}
	//		}
	//	}
	//
	//	for k, v := range *args {
	//	}
	//	args2 = makeLogableData(reflect.ValueOf(args), conf.LogInputArrayNum).Interface()
	//}

	////byteOutHeaders, _ := json.Marshal(outHeaders)
	//if outLen > conf.LogResponseSize && result != nil {
	//	//outBytes = outBytes[0:conf.LogResponseSize]
	//	t := reflect.TypeOf(result)
	//	for t.Kind() == reflect.Ptr {
	//		t = t.Elem()
	//	}
	//	if t.Kind() == reflect.String {
	//		result = result.(string)[0:conf.LogResponseSize]
	//	} else if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
	//		result = result.([]byte)[0:conf.LogResponseSize]
	//	} else {
	//		result = makeLogableData(reflect.ValueOf(result)).Interface()
	//	}
	//}

	var args2 map[string]interface{}
	if args != nil {
		fixedArgs := makeLogableData(reflect.ValueOf(args), nil, conf.LogInputArrayNum, 1).Interface()
		if v, ok := fixedArgs.(map[string]interface{}); ok {
			args2 = v
		} else {
			args2 = map[string]interface{}{"data": args}
		}
	} else {
		args2 = map[string]interface{}{}
	}
	if result != nil {
		result = makeLogableData(reflect.ValueOf(result), &logOutputFields, conf.LogOutputArrayNum, 1).Interface()
	}

	if extraInfo == nil {
		extraInfo = Map{}
	}
	extraInfo["type"] = logName
	//extraInfo["ip"] = getRealIp(request)
	//extraInfo["app"] = conf.App
	//extraInfo["host"] = request.Host
	//extraInfo["server"] = serverAddr
	//extraInfo["method"] = request.Method
	//extraInfo["uri"] = request.RequestURI
	//extraInfo["authLevel"] = authLevel
	//extraInfo["usedTime"] = usedTime
	//extraInfo["status"] = response.status
	//extraInfo["outLen"] = outLen
	//extraInfo["in"] = args2
	//extraInfo["inHeaders"] = headers
	//extraInfo["out"] = result
	//extraInfo["outHeaders"] = outHeaders

	host := request.Header.Get(standard.DiscoverHeaderHost)
	if host == "" {
		host = request.Host
	}

	log.LogRequest(conf.App, serverAddr, getRealIp(request), request.Header.Get(standard.DiscoverHeaderFromApp), request.Header.Get(standard.DiscoverHeaderFromNode), request.Header.Get(standard.DiscoverHeaderClientId), request.Header.Get(standard.DiscoverHeaderSessionId), request.Header.Get(standard.DiscoverHeaderRequestId), host, u.StringIf(request.TLS == nil, "http", "https"), request.Proto[5:], int(authLevel), 0, request.Method, request.RequestURI, *headers, args2, usedTime, response.status, outHeaders, uint(outLen), result, extraInfo)

	//log.Info("S", extraInfo)
	//log.Printf("%s	%s	%s	%s	%s	%s	%d	%.6f	%d	%d	%s	%s	%s	%s	%s", logName, getRealIp(request), u.StringIf(conf.App != "", conf.App, request.Host), serverAddr, request.Method, request.RequestURI, authLevel, usedTime, response.status, outLen, string(byteArgs), string(byteHeaders), string(outBytes), string(byteOutHeaders), request.Proto[5:])
}

func makeLogableData(v reflect.Value, allows *map[string]bool, numArrays int, level int) reflect.Value {
	t := v.Type()
	if t == nil {
		return reflect.ValueOf(nil)
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
		v = v.Elem()
	}

	switch t.Kind() {
	case reflect.Struct:
		v2 := reflect.MakeMap(reflect.TypeOf(Map{}))
		for i := 0; i < v.NumField(); i++ {
			k := t.Field(i).Name
			if level == 1 && allows != nil && (*allows)[strings.ToLower(k)] == false {
				continue
			}
			if requireEncryptField(k) {
				v2.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(encryptField(v.Interface())))
			} else {
				v2.SetMapIndex(reflect.ValueOf(k), makeLogableData(v.Field(i), nil, numArrays, level+1))
			}
		}
		return v2
	case reflect.Map:
		v2 := reflect.MakeMap(t)
		for _, mk := range v.MapKeys() {
			k := mk.String()
			if allows != nil && (*allows)[strings.ToLower(k)] == false {
				continue
			}
			if requireEncryptField(k) {
				v2.SetMapIndex(mk, reflect.ValueOf(encryptField(v.Interface())))
			} else {
				v2.SetMapIndex(mk, makeLogableData(v.MapIndex(mk), nil, numArrays, level+1))
			}
		}
		return v2
	case reflect.Slice:
		if numArrays == 0 {
			var tStr string
			if t.Elem().Kind() == reflect.Interface && v.Len() > 0 {
				tStr = reflect.TypeOf(v.Index(0).Interface()).String()
			} else {
				tStr = t.Elem().String()
			}
			return reflect.ValueOf(fmt.Sprintf("%s (%d)", tStr, v.Len()))
		}
		v2 := reflect.MakeSlice(reflect.TypeOf(Arr{}), 0, 0)
		for i := 0; i < v.Len(); i++ {
			if i >= numArrays {
				break
			}
			v2 = reflect.Append(v2, makeLogableData(v.Index(i), nil, numArrays, level+1))
		}
		return v2
	case reflect.Interface:
		v2 := reflect.ValueOf(v.Interface())
		if v2.Kind() == reflect.Invalid {
			return reflect.ValueOf(nil)
		}
		if v2.Type().Kind() != reflect.Interface {
			return makeLogableData(v2, nil, numArrays, level)
		} else {
			return v2
		}
	default:
		return v
	}
}

func getRealIp(request *http.Request) string {
	return u.StringIf(request.Header.Get(standard.DiscoverHeaderClientIp) != "", request.Header.Get(standard.DiscoverHeaderClientIp), request.RemoteAddr[0:strings.IndexByte(request.RemoteAddr, ':')])
}

/* ================================================================================= */
type GzipResponseWriter struct {
	*Response
	zipWriter *gzip.Writer
}

func (gzw *GzipResponseWriter) Write(b []byte) (int, error) {
	contentLen, err := gzw.zipWriter.Write(b)
	gzw.zipWriter.Flush()
	return contentLen, err
}

func (gzw *GzipResponseWriter) Close() {
	gzw.zipWriter.Close()
}

func NewGzipResponseWriter(w *Response) *GzipResponseWriter {
	w.Header().Set("Content-Encoding", "gzip")

	gz := gzip.NewWriter(w)

	gzw := GzipResponseWriter{
		zipWriter: gz,
		Response:  w,
	}

	return &gzw
}
