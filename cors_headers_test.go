package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProjectPaginationHeaderIsExposedForAllowedOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	route := gin.New()
	route.Use(func(c *gin.Context) {
		c.Header("Access-Control-Expose-Headers", "Content-Language, Vary")
		c.Next()
	})
	route.Use(libwkhttp.SecureCORSOverrideMiddleware([]string{"https://web.example"}))
	route.Use(exposeProjectPaginationHeader())
	route.GET("/projects", func(c *gin.Context) {
		c.Header("X-Total-Count", "3")
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.Header.Set("Origin", "https://web.example")
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "https://web.example", w.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "Content-Language")
	require.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "Vary")
	require.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "X-Total-Count")
}

func TestProjectPaginationHeaderIsNotExposedWhenCorsIsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	route := gin.New()
	route.Use(libwkhttp.SecureCORSOverrideMiddleware(nil))
	route.Use(exposeProjectPaginationHeader())
	route.GET("/projects", func(c *gin.Context) {
		c.Header("X-Total-Count", "3")
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
	require.Empty(t, w.Header().Get("Access-Control-Expose-Headers"),
		"the pagination middleware must not restore a CORS header after CORS is disabled")
}
