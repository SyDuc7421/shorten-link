package handler

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"

	"github.com/april/shorten-link/api"
)

const swaggerVersion = "5.17.14"

const swaggerInit = `
  SwaggerUIBundle({
    url: "/api/openapi.yaml",
    dom_id: "#swagger-ui",
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
    layout: "BaseLayout",
    deepLinking: true,
  });
`

// docsCSP relaxes the global default-src 'none' just enough for Swagger UI:
// pinned unpkg assets plus the inline init script, allowed by its hash.
var docsCSP = func() string {
	sum := sha256.Sum256([]byte(swaggerInit))
	return "default-src 'none'; " +
		"script-src https://unpkg.com 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; " +
		"style-src https://unpkg.com 'unsafe-inline'; " + // Swagger UI sets inline style attributes
		"img-src 'self' data:; " +
		"connect-src 'self'"
}()

// RegisterDocs mounts the Swagger UI at GET /docs and the raw spec at GET /api/openapi.yaml.
func RegisterDocs(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(api.Spec) //nolint:errcheck
	})

	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", docsCSP)
		w.Write([]byte(swaggerUI)) //nolint:errcheck
	})
}

const swaggerUI = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>Short Link API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@` + swaggerVersion + `/swagger-ui.css"/>
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@` + swaggerVersion + `/swagger-ui-bundle.js"></script>
<script>` + swaggerInit + `</script>
</body>
</html>`
