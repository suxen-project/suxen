// Command tls-proxy is a test-only TLS terminator for real-client examples.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

func main() {
	listen := flag.String("listen", "", "TLS listen address")
	targetValue := flag.String("target", "", "upstream HTTP URL")
	certificate := flag.String("cert", "", "TLS certificate")
	key := flag.String("key", "", "TLS private key")
	flag.Parse()
	target, err := url.Parse(*targetValue)
	if err != nil {
		log.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(request *http.Request) {
		director(request)
		request.Header.Set("X-Forwarded-Proto", "https")
	}
	log.Fatal(http.ListenAndServeTLS(*listen, *certificate, *key, proxy))
}
