package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/hashicorp/consul/api"
)

var (
	consulHost = flag.String("consul.host", "10.10.10.107", "consul server ip address")
	consulPort = flag.String("consul.port", "8500", "consul server port")
)

type ResponseMap struct {
	Msg  string
	Code int
}

var responseMap ResponseMap

/**
 *
 */
func main() {
	flag.Parse()

	var logger log.Logger
	{
		logger = log.NewLogfmtLogger(os.Stderr)
		logger = log.With(logger, "ts", log.DefaultTimestampUTC)
		logger = log.With(logger, "caller", log.DefaultCaller)
	}

	consulCfg := api.DefaultConfig()
	consulCfg.Address = "http://" + *consulHost + ":" + *consulPort
	consulClient, err := api.NewClient(consulCfg)

	if err != nil {
		logger.Log("err", err)
		os.Exit(1)
	}

	proxy := NewReverseProxy(consulClient, logger)

	errChan := make(chan error)
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		errChan <- fmt.Errorf("%s", <-c)
	}()

	go func() {
		logger.Log("transport", "http", "addr", "8003")
		handler := proxy
		errChan <- http.ListenAndServe(":8003", handler)
	}()

	logger.Log("exit", <-errChan)
}

var transport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second, //连接超时
		KeepAlive: 30 * time.Second, //长连接超时时间
	}).DialContext,
	MaxIdleConns:          100,              //最大空闲连接
	IdleConnTimeout:       90 * time.Second, //空闲超时时间
	TLSHandshakeTimeout:   10 * time.Second, //tls握手超时时间
	ExpectContinueTimeout: 1 * time.Second,  //100-continue 超时时间
}

// NewReverseProxy 反向代理
func NewReverseProxy(client *api.Client, logger log.Logger) *httputil.ReverseProxy {

	// proxy Director 请求协调者  对请求进行设置 修改
	director := func(r *http.Request) {

		//查询原始请求路径，如：/user/users/5
		reqPath := r.URL.Path
		logger.Log("request Path:", reqPath)

		if reqPath == "" {
			return
		}

		//按照分隔符'/'对路径进行分解，获取服务名称serviceName
		pathArray := strings.Split(reqPath, "/")
		serviceName := pathArray[1]
		logger.Log("serviceName:", serviceName)

		//调用consul api查询serviceName的服务实例列表
		result, _, err := client.Catalog().Service(serviceName, "", nil)
		if err != nil {
			logger.Log("reverseProxy failed", "query service instance error", err.Error())
			return
		}

		if len(result) == 0 {
			logger.Log("reverseProxy failed", "no such service instance", serviceName)
			return
		}

		//重新组织请求路径，去掉服务名称部分
		destPath := strings.Join(pathArray[2:], "/")

		//随机选择一个服务实例
		tgt := result[rand.Int()%len(result)]
		logger.Log("service id", tgt.ServiceID)

		//设置代理服务地址信息
		r.URL.Scheme = "http"
		r.URL.Host = fmt.Sprintf("%s:%d", tgt.ServiceAddress, tgt.ServicePort)
		r.URL.Path = "/" + destPath

		// TODO: 如要在API网关当中加入 token的验证，在此处解析完token之后，以Header 参数进行传递
		//只在第一代理中设置此header头
		r.Header.Set("X-Real-Ip", r.RemoteAddr)
	}

	//更改内容
	modifyFunc := func(resp *http.Response) error {
		//请求以下命令：curl 'http://127.0.0.1:2002/error'
		if resp.StatusCode != 200 || resp.StatusCode != 201 || resp.StatusCode != 203 || resp.StatusCode != 204 {
			//获取内容
			oldPayload, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			// body 追加内容
			newPayload := []byte("" + string(oldPayload))
			resp.Body = ioutil.NopCloser(bytes.NewBuffer(newPayload))

			// head 修改追加内容
			resp.ContentLength = int64(len(newPayload))
			resp.Header.Set("Content-Length", strconv.FormatInt(int64(len(newPayload)), 10))
		}
		return nil
	}

	errFunc := func(w http.ResponseWriter, r *http.Request, err error) {
		//查询原始请求路径，如：/user/users/5
		reqPath := r.URL.Path

		if reqPath == "" {
			return
		}

		//按照分隔符'/'对路径进行分解，获取服务名称serviceName
		pathArray := strings.Split(reqPath, "/")
		serviceName := pathArray[1]
		//调用consul api查询serviceName的服务实例列表
		result, _, err := client.Catalog().Service(serviceName, "", nil)
		if err != nil {
			logger.Log("reverseProxy failed", "query service instance error", err.Error())
			return
		}

		if len(result) == 0 {
			http.Error(w, serviceName + " Not Found", 404)
			return
		}

		http.Error(w, ""+err.Error(), 500)
	}

	return &httputil.ReverseProxy{
		Director:       director,   //请求内容
		Transport:      transport,  //连接池
		ModifyResponse: modifyFunc, //用于修改响应结果及HTTP状态码，当返回结果error不为空时，会调用ErrorHandler
		ErrorHandler:   errFunc,    //用于处理后端和ModifyResponse返回的错误信息，默认将返回传递过来的错误信息，并返回HTTP 502
	}
}
