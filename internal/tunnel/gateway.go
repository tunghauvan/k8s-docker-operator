package tunnel

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// RunGateway starts a simple reverse proxy gateway.
// It listens on listenAddr and proxies requests based on the 'target' query parameter.
// Example: /connect?target=service-name:port -> http://service-name:port/connect
func RunGateway(listenAddr string) error {
	log.Printf("Starting Tunnel Gateway on %s", listenAddr)

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			target := req.URL.Query().Get("target")
			if target == "" {
				log.Println("Error: missing target query parameter")
				return
			}

			// Try parsing target as is
			targetURL, err := url.Parse(target)
			if err != nil || targetURL.Scheme == "" || targetURL.Host == "" {
				// Try prepending http:// if parsing failed or no scheme/host
				targetURL, err = url.Parse("http://" + target)
				if err != nil {
					log.Printf("Error parsing target URL %s: %v", target, err)
					return
				}
			}

			// Convert ws/wss to http/https for ReverseProxy
			if targetURL.Scheme == "ws" {
				targetURL.Scheme = "http"
			} else if targetURL.Scheme == "wss" {
				targetURL.Scheme = "https"
			}

			log.Printf("Proxying request to %s", targetURL)

			// Update the request to point to the target
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host

			// Use the path and query from the target URL
			if targetURL.Path != "" {
				req.URL.Path = targetURL.Path
			}

			// Set query: default to target's query, but append 'token' from original request if present
			targetQuery := targetURL.Query()
			originalQuery := req.URL.Query()
			if token := originalQuery.Get("token"); token != "" {
				targetQuery.Set("token", token)
			}
			req.URL.RawQuery = targetQuery.Encode()

			// Important: Set Host header so the backend knows who it is
			req.Host = targetURL.Host
		},
	}

	http.Handle("/", proxy)
	return http.ListenAndServe(listenAddr, nil)
}
