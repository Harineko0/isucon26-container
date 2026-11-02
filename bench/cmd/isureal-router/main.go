// isureal-router は競技のルーターを手元で再現する。
//
//   - X-Country-Code で asia / europe / america のサーバに振り分ける (省略時は JP = asia)。
//     空値・未対応の値・小文字・複数値は 400 Bad Request (manual.md「通信経路」)。
//   - X-Client-IP が無ければ接続元 IP を入れる。あればそのまま通す。
//   - -latency で、リージョンごとに片道の遅延を足せる (ベンチ ↔ EU/US サーバの距離の再現)。
//     サーバ ↔ DB の遅延はアプリのコンテナ側 (tc netem) で付ける。
//   - SSE (GET /api/notifications) はバッファせずに流す。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

var regionOf = map[string]string{
	"JP": "asia", "KR": "asia", "CN": "asia", "AU": "asia",
	"GB": "europe", "DE": "europe", "FR": "europe",
	"US": "america", "CA": "america", "MX": "america",
}

type backend struct {
	proxy   *httputil.ReverseProxy
	latency time.Duration
}

func main() {
	listen := flag.String("listen", ":8080", "待ち受けアドレス")
	asia := flag.String("asia", "http://192.168.1.10", "asia1 (nginx) の URL")
	europe := flag.String("europe", "http://192.168.2.10", "europe1 の URL")
	america := flag.String("america", "http://192.168.3.10", "america1 の URL")
	latency := flag.String("latency", "", "リージョンごとの片道遅延 (例 europe=50ms,america=50ms)")
	verbose := flag.Bool("v", false, "リクエストをログに出す")
	flag.Parse()

	backends := map[string]*backend{}
	for region, raw := range map[string]string{"asia": *asia, "europe": *europe, "america": *america} {
		target, err := url.Parse(raw)
		if err != nil {
			log.Fatalf("invalid url for %s: %v", region, err)
		}
		p := httputil.NewSingleHostReverseProxy(target)
		p.FlushInterval = -1 // SSE
		p.Transport = &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        4096,
			MaxIdleConnsPerHost: 4096,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		}
		director := p.Director
		p.Director = func(r *http.Request) {
			director(r)
			r.Host = target.Host
		}
		p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		}
		backends[region] = &backend{proxy: p}
	}
	if *latency != "" {
		for _, kv := range strings.Split(*latency, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok {
				log.Fatalf("invalid -latency %q", kv)
			}
			d, err := time.ParseDuration(v)
			if err != nil {
				log.Fatalf("invalid -latency %q: %v", kv, err)
			}
			if b := backends[k]; b != nil {
				b.latency = d
			}
		}
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codes := r.Header.Values("X-Country-Code")
		code := "JP"
		if len(codes) > 1 {
			http.Error(w, "Bad Request: multiple X-Country-Code", http.StatusBadRequest)
			return
		}
		if len(codes) == 1 {
			code = codes[0]
		}
		region, ok := regionOf[code]
		if !ok {
			http.Error(w, "Bad Request: invalid X-Country-Code", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Client-IP") == "" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			r.Header.Set("X-Client-IP", host)
		}
		b := backends[region]
		if b.latency > 0 {
			time.Sleep(b.latency)
			w = &delayedWriter{ResponseWriter: w, delay: b.latency}
		}
		if *verbose {
			log.Printf("%s %s %s -> %s", r.Header.Get("X-Client-IP"), r.Method, r.URL.Path, region)
		}
		b.proxy.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	log.Printf("isureal-router listening on %s (asia=%s europe=%s america=%s latency=%s)", *listen, *asia, *europe, *america, *latency)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// delayedWriter は応答の先頭を遅らせる (復路の遅延)。
type delayedWriter struct {
	http.ResponseWriter
	delay   time.Duration
	started bool
}

func (d *delayedWriter) WriteHeader(code int) {
	if !d.started {
		d.started = true
		time.Sleep(d.delay)
	}
	d.ResponseWriter.WriteHeader(code)
}

func (d *delayedWriter) Write(p []byte) (int, error) {
	if !d.started {
		d.WriteHeader(http.StatusOK)
	}
	return d.ResponseWriter.Write(p)
}

func (d *delayedWriter) Flush() {
	if f, ok := d.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

var _ = fmt.Sprint
