package main

import (
	"github.com/valyala/fasthttp"
	RestTunnel "github.com/TheRockettek/RestTunnel/internal"
)

func main() {
	restTunnel := RestTunnel{}
	err := fasthttp.ListenAndServe("0.0.0.0:80", restTunnel.HandleRequest)
	if err != nil {
		println(err.Error())
	}
}
