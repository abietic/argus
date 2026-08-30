package platformapi

import (
	"embed"
	"fmt"
	"net/http"
	"strconv"
)

//go:embed ui/index.html ui/app.css ui/app.js
var operatorUI embed.FS

var operatorUIAssets = map[string]struct {
	path        string
	contentType string
}{
	"/ui/":        {path: "ui/index.html", contentType: "text/html; charset=utf-8"},
	"/ui/app.css": {path: "ui/app.css", contentType: "text/css; charset=utf-8"},
	"/ui/app.js":  {path: "ui/app.js", contentType: "text/javascript; charset=utf-8"},
}

// serveOperatorUI serves only a credential-free static shell. It is the sole
// unauthenticated surface and never injects principal, token or store data.
// Every data mutation/query performed by the shell still traverses the normal
// authenticated API routes.
func serveOperatorUI(writer http.ResponseWriter, request *http.Request) bool {
	if request.URL.RawPath != "" {
		return false
	}
	if request.URL.Path == "/" || request.URL.Path == "/ui" {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writeStaticMethodNotAllowed(writer)
			return true
		}
		http.Redirect(writer, request, "/ui/", http.StatusTemporaryRedirect)
		return true
	}
	asset, exists := operatorUIAssets[request.URL.Path]
	if !exists {
		return false
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writeStaticMethodNotAllowed(writer)
		return true
	}
	data, err := operatorUI.ReadFile(asset.path)
	if err != nil {
		http.Error(writer, "operator UI unavailable", http.StatusInternalServerError)
		return true
	}
	setStaticSecurityHeaders(writer)
	writer.Header().Set("Content-Type", asset.contentType)
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = writer.Write(data)
	}
	return true
}

func setStaticSecurityHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	writer.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("X-Frame-Options", "DENY")
}

func writeStaticMethodNotAllowed(writer http.ResponseWriter) {
	setStaticSecurityHeaders(writer)
	writer.Header().Set("Allow", fmt.Sprintf("%s, %s", http.MethodGet, http.MethodHead))
	http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
}
