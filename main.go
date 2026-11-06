// dsh-sidecar 是运行在 dsh 服务器上的独立中继。
//
// 当前版本（v0）只做 HTTP 中继，不含 TLS 与用户认证：
//   - 从 ~/.dsh/.credentials.yaml 读取内核浏览器会话密钥，代浏览器签发 dsh-auth-* Cookie；
//   - 把 Host/Origin 归一化为上游回环地址，满足内核的 loopback trust fence；
//   - 在 index.html 里注入 __DSH_TRANSPORT__.ownsHost=true，让远程页面按本机处理；
//   - 透传 HTTP、静态资源与 WebSocket 升级。
//
// 用法：
//
//	go run . -listen 127.0.0.1:8088 -upstream http://127.0.0.1:3080
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	authCookiePrefix = "dsh-auth-"
	defaultTTL       = 7 * 24 * time.Hour

	// 内核允许的会话 Cookie 上限，超过即视为无效。
	maxCookieAge = 30 * 24 * time.Hour

	// 让客户端把本页当作"本机"，与 dsh-remote 的 tapIndex 注入等价；重复注入无副作用。
	transportShim = `globalThis.__DSH_TRANSPORT__=Object.assign({},globalThis.__DSH_TRANSPORT__,{ownsHost:true});`

	// ReMe 状态页在浏览器侧直连 http://127.0.0.1:2333：既跨源（会触发 OPTIONS 预检），
	// 又指向用户自己的回环地址。这里把发往该地址的 fetch 改写到同源 /reme/*，
	// 由本中继转发到真实的 ReMe 服务；服务端插件仍直连 2333，不受影响。
	remeShim = `(function(){var R=['http://127.0.0.1:2333','http://localhost:2333'];var f=globalThis.fetch;if(!f)return;globalThis.fetch=function(i,o){try{var u=typeof i==='string'?i:(i&&i.url)?i.url:String(i);for(var k=0;k<R.length;k++){if(u.indexOf(R[k])===0){var rest=u.slice(R[k].length).replace(/^\/+/,'');var t='/reme/'+rest;i=(typeof Request!=='undefined'&&i instanceof Request)?new Request(t,i):t;break;}}}catch(e){}return f.call(globalThis,i,o);};})();`
)

// remeJobs 放行状态页用到的作业。
// auto_dream 是"立即整理"按钮背后的写操作，默认开放；用 -reme-readonly 关掉。
// auto_memory 只由 dsh 服务端调用（直连 2333），浏览器侧不需要，故始终不放行。
var remeJobs = map[string]bool{
	"status":       true,
	"health_check": true,
	"app_config":   true,
	"list":         true,
	"load":         true,
	"search":       true,
	"auto_dream":   true,
}

// headTag 匹配 <head> 或 <head ...>
var headTag = regexp.MustCompile(`(?i)<head(?:\s[^>]*)?>`)

type sidecar struct {
	upstream    *url.URL
	authority   string // 上游 authority，用于 Cookie 绑定与 Host 改写
	secret      []byte
	cookieName  string
	cookieValue string
	inject      bool
	verbose     bool
	remeAuth    bool
	shim        []byte
	remeProxy   *httputil.ReverseProxy
}

// options 汇总可配置项。优先级：命令行 > YAML 配置文件 > 默认值。
type options struct {
	Listen       string
	Upstream     string
	Reme         string
	DSHHome      string
	Credentials  string
	CookieTTL    time.Duration
	Inject       bool
	RemeAuth     bool
	RemeReadonly bool
	Verbose      bool
	ConfigPath   string
}

// fileConfig 是 YAML 配置文件的结构。字段用指针，以便区分“未出现”和“显式零值”。
type fileConfig struct {
	Listen       *string `yaml:"listen"`
	Upstream     *string `yaml:"upstream"`
	Reme         *string `yaml:"reme"`
	DSHHome      *string `yaml:"dsh-home"`
	Credentials  *string `yaml:"credentials"`
	CookieTTL    *string `yaml:"cookie-ttl"`
	Inject       *bool   `yaml:"inject"`
	RemeAuth     *bool   `yaml:"reme-auth"`
	RemeReadonly *bool   `yaml:"reme-readonly"`
	Verbose      *bool   `yaml:"verbose"`
}

// defaultConfigPath 返回自动探测的配置路径；无法确定 HOME 时返回空串。
func defaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "dsh-sidecar", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dsh-sidecar", "config.yaml")
}

// loadFileConfig 读取 YAML 并把未在命令行显式给出的项写入 o。
// 自动探测路径不存在时静默跳过；通过 -config 指定的路径不存在则报错。
func loadFileConfig(o *options) error {
	path := o.ConfigPath
	auto := path == ""
	if auto {
		path = defaultConfigPath()
		if path == "" {
			return nil
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("检查配置文件失败: %w", err)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		return fmt.Errorf("解析配置文件 %s: %w", path, err)
	}

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if fc.Listen != nil && !set["listen"] {
		o.Listen = *fc.Listen
	}
	if fc.Upstream != nil && !set["upstream"] {
		o.Upstream = *fc.Upstream
	}
	if fc.Reme != nil && !set["reme"] {
		o.Reme = *fc.Reme
	}
	if fc.DSHHome != nil && !set["dsh-home"] {
		o.DSHHome = *fc.DSHHome
	}
	if fc.Credentials != nil && !set["credentials"] {
		o.Credentials = *fc.Credentials
	}
	if fc.CookieTTL != nil && !set["cookie-ttl"] {
		ttl, err := time.ParseDuration(*fc.CookieTTL)
		if err != nil {
			return fmt.Errorf("cookie-ttl 无效（示例 168h）: %w", err)
		}
		o.CookieTTL = ttl
	}
	if fc.Inject != nil && !set["inject"] {
		o.Inject = *fc.Inject
	}
	if fc.RemeAuth != nil && !set["reme-auth"] {
		o.RemeAuth = *fc.RemeAuth
	}
	if fc.RemeReadonly != nil && !set["reme-readonly"] {
		o.RemeReadonly = *fc.RemeReadonly
	}
	if fc.Verbose != nil && !set["v"] {
		o.Verbose = *fc.Verbose
	}

	log.Printf("已加载配置文件: %s", path)
	return nil
}

// resolveCredentials 决定 .credentials.yaml 的位置：
// -credentials 优先，其次 -dsh-home / $DSH_HOME，最后 ~/.dsh。
func resolveCredentials(o options) (string, error) {
	if o.Credentials != "" {
		return o.Credentials, nil
	}
	home := o.DSHHome
	if home == "" {
		home = os.Getenv("DSH_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法确定 HOME: %w", err)
		}
		home = filepath.Join(userHome, ".dsh")
	}
	return filepath.Join(home, ".credentials.yaml"), nil
}

func main() {
	var o options
	flag.StringVar(&o.Listen, "listen", "127.0.0.1:8088", "监听地址")
	flag.StringVar(&o.Upstream, "upstream", "http://127.0.0.1:3080", "dsh web 上游地址")
	flag.StringVar(&o.Reme, "reme", "http://127.0.0.1:2333", "ReMe 上游地址（浏览器侧经 /reme/* 转发）")
	flag.StringVar(&o.DSHHome, "dsh-home", "", "DSH_HOME 目录（默认 $DSH_HOME 或 ~/.dsh）")
	flag.StringVar(&o.Credentials, "credentials", "", "内核会话密钥文件路径，优先于 -dsh-home")
	flag.DurationVar(&o.CookieTTL, "cookie-ttl", defaultTTL, "代签 Cookie 有效期（上限 30 天）")
	flag.BoolVar(&o.Inject, "inject", true, "向 index.html 注入 ownsHost 与 ReMe 改写脚本")
	flag.BoolVar(&o.RemeAuth, "reme-auth", true, "要求 /reme/* 携带有效的内核会话 Cookie")
	flag.BoolVar(&o.RemeReadonly, "reme-readonly", false, "禁止浏览器侧触发 auto_dream")
	flag.BoolVar(&o.Verbose, "v", false, "打印每个请求")
	flag.StringVar(&o.ConfigPath, "config", "", "YAML 配置文件；留空则探测 $XDG_CONFIG_HOME/dsh-sidecar/config.yaml")
	flag.Parse()

	if err := loadFileConfig(&o); err != nil {
		log.Fatal(err)
	}

	up, err := url.Parse(o.Upstream)
	if err != nil || up.Host == "" {
		log.Fatalf("upstream 地址无效: %v", err)
	}
	reme, err := url.Parse(o.Reme)
	if err != nil || reme.Host == "" {
		log.Fatalf("reme 地址无效: %v", err)
	}
	if o.RemeReadonly {
		delete(remeJobs, "auto_dream")
	}

	credPath, err := resolveCredentials(o)
	if err != nil {
		log.Fatal(err)
	}
	secret, err := loadSecret(credPath)
	if err != nil {
		log.Fatalf("读取内核会话密钥失败（%s）: %v", credPath, err)
	}

	s := &sidecar{
		upstream:  up,
		authority: up.Host,
		secret:    secret,
		inject:    o.Inject,
		verbose:   o.Verbose,
		remeAuth:  o.RemeAuth,
		shim:      []byte("<script>" + transportShim + "</script><script>" + remeShim + "</script>"),
	}
	s.cookieName, s.cookieValue = mintCookie(s.secret, s.authority, o.CookieTTL)
	s.remeProxy = buildRemeProxy(reme)

	srv := &http.Server{
		Addr:              o.Listen,
		Handler:           s.handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	log.Printf("dsh-sidecar 启动: listen=%s upstream=%s reme=%s credentials=%s cookie=%s ttl=%s inject=%v reme-write=%v reme-auth=%v",
		o.Listen, up.String(), reme.String(), credPath, s.cookieName, o.CookieTTL.String(), o.Inject, !o.RemeReadonly, o.RemeAuth)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// handler 组装反向代理：Director 负责改写与代签，ModifyResponse 负责 HTML 注入。
func (s *sidecar) handler() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(s.upstream)
			pr.Out.Host = s.authority
			s.rewriteHeaders(pr.Out)
		},
		ModifyResponse: s.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("上游错误 %s %s: %v", r.Method, r.URL.Path, err)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "dsh-sidecar: upstream error\n")
		},
		FlushInterval: -1, // 流式输出立即刷新
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		switch {
		case r.URL.Path == "/_sidecar/health":
			_, _ = io.WriteString(rec, "ok\n")
			return
		case strings.HasPrefix(r.URL.Path, "/reme/"):
			s.serveReme(rec, r)
		default:
			proxy.ServeHTTP(rec, r)
		}
		if s.verbose {
			log.Printf("%s %s -> %d (%s)", r.Method, r.URL.RequestURI(), rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

// serveReme 把浏览器的 /reme/<job> 转发到 ReMe。
// 先校验内核会话 Cookie：它由 dsh-remote 仅在认证通过后签发给浏览器，
// 因此"签名有效 + authority 匹配 + 未过期"等价于"该浏览器已通过登录"。
func (s *sidecar) serveReme(w http.ResponseWriter, r *http.Request) {
	if s.remeAuth && !s.hasCoreSession(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "{\"ok\":false,\"error\":\"unauthorized\"}\n")
		return
	}
	job := strings.TrimPrefix(r.URL.Path, "/reme/")
	if !remeJobs[job] {
		http.Error(w, "dsh-sidecar: reme job not allowed", http.StatusForbidden)
		return
	}

	// 纯中继：不附加超时，也不改写上下文。客户端取消（刷新/关闭页面）按 HTTP 语义透传。
	s.remeProxy.ServeHTTP(w, r)
}

// buildRemeProxy 去掉 /reme 前缀后转发到 ReMe，不做 Cookie 与 Host 改写。
func buildRemeProxy(reme *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = reme.Scheme
			pr.Out.URL.Host = reme.Host
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/reme")
			pr.Out.URL.RawPath = ""
			pr.Out.Host = reme.Host
			pr.Out.Header.Del("Cookie")
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) && r.Context().Err() == nil {
				// 作业本身被上游或超时中止，而非客户端行为
				log.Printf("ReMe 作业被中止 %s %s: %v", r.Method, r.URL.Path, err)
			} else {
				log.Printf("ReMe 上游错误 %s %s: %v", r.Method, r.URL.Path, err)
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "dsh-sidecar: reme upstream error\n")
		},
		FlushInterval: -1,
	}
}

// rewriteHeaders 把请求改造成"从回环地址发起"，并替换为代签的会话 Cookie。
func (s *sidecar) rewriteHeaders(req *http.Request) {
	// Origin 与 Host 一并归一化：内核的 browser-trust fence 只看这两个。
	if req.Header.Get("Origin") != "" {
		req.Header.Set("Origin", "http://"+s.authority)
	}
	// 去掉可能被上游误判的转发头，再补上标准值。
	req.Header.Del("X-Forwarded-Host")
	req.Header.Del("X-Forwarded-Proto")
	if req.Header.Get("X-Forwarded-For") == "" {
		if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			req.Header.Set("X-Forwarded-For", host)
		}
	}

	// 丢弃上游带来的同名旧 Cookie，换成我们的；其他 Cookie 原样保留。
	cookies := req.Cookies()
	req.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != s.cookieName {
			req.AddCookie(c)
		}
	}
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: s.cookieValue})

	// 关掉压缩，ModifyResponse 才能安全改写 HTML。
	req.Header.Set("Accept-Encoding", "identity")
}

// modifyResponse 只改写 HTML，静态资源与 API 一律原样透传。
func (s *sidecar) modifyResponse(resp *http.Response) error {
	s.maybeClearSessionCookie(resp)
	if !s.inject || resp.Request.Method == http.MethodHead {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	out := headTag.ReplaceAllFunc(body, func(m []byte) []byte {
		return append(append([]byte{}, m...), s.shim...)
	})
	if len(out) == len(body) { // 没有 <head>：退回前置注入
		out = append(append([]byte{}, s.shim...), body...)
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("ETag") // 正文已变，旧校验码失效
	return nil
}

// mintCookie 复刻内核对浏览器会话 Cookie 的约定：
// 名称 = dsh-auth-<base64url(sha256(authority))>，值 = v1.<base64url(JSON)>.<base64url(HMAC-SHA256)>。
func mintCookie(secret []byte, authority string, ttl time.Duration) (name, value string) {
	sum := sha256.Sum256([]byte(authority))
	name = authCookiePrefix + base64.RawURLEncoding.EncodeToString(sum[:])

	now := time.Now().UnixMilli()
	payload := struct {
		Version   int    `json:"version"`
		Authority string `json:"authority"`
		IssuedAt  int64  `json:"issuedAt"`
		ExpiresAt int64  `json:"expiresAt"`
	}{1, authority, now, now + ttl.Milliseconds()}

	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return name, "v1." + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// hasCoreSession 检查请求里是否带有效的内核会话 Cookie。
func (s *sidecar) hasCoreSession(r *http.Request) bool {
	for _, c := range r.Cookies() {
		if c.Name == s.cookieName {
			return verifyCoreCookie(c.Value, s.secret, s.authority)
		}
	}
	return false
}

// verifyCoreCookie 校验 v1.<b64url(JSON)>.<b64url(HMAC)> 的签名、authority 与有效期。
func verifyCoreCookie(value string, secret []byte, authority string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var payload struct {
		Version   int    `json:"version"`
		Authority string `json:"authority"`
		IssuedAt  int64  `json:"issuedAt"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return false
	}
	now := time.Now().UnixMilli()
	if payload.Version != 1 || payload.Authority != authority {
		return false
	}
	if payload.IssuedAt > now || payload.ExpiresAt <= now || payload.ExpiresAt <= payload.IssuedAt {
		return false
	}
	return payload.ExpiresAt-payload.IssuedAt <= maxCookieAge.Milliseconds()
}

// maybeClearSessionCookie 在登出成功后清掉浏览器侧的内核 Cookie，
// 否则登出后那条 Cookie 仍然有效，/reme 依旧放行。
func (s *sidecar) maybeClearSessionCookie(resp *http.Response) {
	if resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.Path != "/auth/logout" {
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	resp.Header.Add("Set-Cookie", s.cookieName+"=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict")
}

// loadSecret 从凭据文件里取出 client-connection/browser-session 的密钥（base64url，32 字节）。
func loadSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(data)
	idx := strings.Index(text, "client-connection/browser-session:")
	if idx < 0 {
		return nil, fmt.Errorf("%s 中没有 client-connection/browser-session 记录", path)
	}
	block := text[idx:]
	re := regexp.MustCompile(`(?m)^\s+secret:\s*([A-Za-z0-9_\-+/=]+)\s*$`)
	m := re.FindStringSubmatch(block)
	if m == nil {
		return nil, errors.New("该记录缺少 secret 字段")
	}
	secret, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(m[1], "="))
	if err != nil {
		return nil, fmt.Errorf("secret 不是合法的 base64url: %w", err)
	}
	if len(secret) != 32 {
		return nil, fmt.Errorf("secret 长度异常: %d", len(secret))
	}
	return secret, nil
}

// statusRecorder 记录状态码，同时保留 Hijack/Flush 能力（WebSocket 升级必需）。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("底层 ResponseWriter 不支持 Hijack")
	}
	r.status = http.StatusSwitchingProtocols
	return hijacker.Hijack()
}

func (r *statusRecorder) Flush() {
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}
