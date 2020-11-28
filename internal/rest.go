package resttunnel

import (
	"net/http"

	"github.com/gorilla/mux"
)

// NewMethodRouter creates a new method router.
func NewMethodRouter() *MethodRouter {
	return &MethodRouter{mux.NewRouter()}
}

// MethodRouter beepboop.
type MethodRouter struct {
	*mux.Router
}

// HandleFunc registers a route that handles both paths and methods.
func (mr *MethodRouter) HandleFunc(path string,
	f func(http.ResponseWriter, *http.Request), methods ...string) *mux.Route {
	if len(methods) == 0 {
		methods = []string{"GET"}
	}

	return mr.NewRoute().Path(path).Methods(methods...).HandlerFunc(f)
}

func createEndpoints(rt *RestTunnel) (router *MethodRouter) {
	router = NewMethodRouter()

	router.HandleFunc("/resttunnel", AliveHandler(rt), "GET")
	router.HandleFunc("/resttunnel/analytics", AnalyticsHandler(rt), "GET")
	router.HandleFunc("/resttunnel/callbacks/{id}", CallbacksHandler(rt), "GET")

	return
}
