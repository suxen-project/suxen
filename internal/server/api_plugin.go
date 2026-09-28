package server

import (
	"net/http"

	"github.com/suxen-project/suxen/internal/httpx"
	spiapi "github.com/suxen-project/suxen/spi/api"
)

type pluginAPIContext struct {
	pluginID   string
	parameters map[string]string
	writer     http.ResponseWriter
	request    *http.Request
}

func (ctx pluginAPIContext) PluginID() string { return ctx.pluginID }

func (ctx pluginAPIContext) Parameters() map[string]string { return ctx.parameters }

func (ctx pluginAPIContext) Request() *http.Request { return ctx.request }

func (ctx pluginAPIContext) ResponseWriter() http.ResponseWriter { return ctx.writer }

func (ctx pluginAPIContext) DecodeJSON(destination any) bool {
	return httpx.DecodeJSON(ctx.writer, ctx.request, destination)
}

func (ctx pluginAPIContext) WriteJSON(status int, value any) {
	httpx.WriteJSON(ctx.writer, status, value)
}

func (ctx pluginAPIContext) WriteProblem(status int, code, message string) {
	httpx.WriteProblem(ctx.writer, status, code, message)
}

var _ spiapi.Context = pluginAPIContext{}
