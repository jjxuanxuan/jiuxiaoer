package docs

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed openapi.yaml
var openAPIDoc []byte

// RegisterRoutes 注册Routes。
func RegisterRoutes(router *gin.RouterGroup) {
	router.GET("/docs/openapi.yaml", OpenAPI)
	router.GET("/swagger", SwaggerRedirect)
	router.GET("/swagger/index.html", SwaggerUI)
}

// OpenAPI 解密并返回API。
func OpenAPI(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", openAPIDoc)
}

// SwaggerRedirect 处理Swagger Redirect相关逻辑。
func SwaggerRedirect(c *gin.Context) {
	c.Redirect(http.StatusFound, "/api/v1/swagger/index.html")
}

// SwaggerUI 处理Swagger UI相关逻辑。
func SwaggerUI(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(swaggerHTML))
}

const swaggerHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <title>九小二 P0 API Swagger</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: "/api/v1/docs/openapi.yaml",
      dom_id: "#swagger-ui",
      deepLinking: true,
      presets: [SwaggerUIBundle.presets.apis],
      layout: "BaseLayout"
    });
  </script>
</body>
</html>`
