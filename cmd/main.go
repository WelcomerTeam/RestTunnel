package main

import (
	resttunnel "../internal"
	"github.com/valyala/fasthttp"
)

func main() {
	restTunnel := resttunnel.RestTunnel{}
	println("Listening on tunnel")
	err := fasthttp.ListenAndServe("0.0.0.0:8000", restTunnel.HandleRequest)
	if err != nil {
		println(err.Error())
	}
}
