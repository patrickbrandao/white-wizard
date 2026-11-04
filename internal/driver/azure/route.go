package azure

import (
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Path families of the Azure OpenAI data plane (spec 004, spec 006 16.6).
const (
	deploymentsPrefix = "/openai/deployments/"
	v1Prefix          = "/openai/v1/"
)

// family is the API a request reached.
type family int

const (
	// familyUnknown: no x-wwz-path (contract tests, simulate): the kind of
	// the route decides and the model comes from the body.
	familyUnknown family = iota
	// familyDeployment: /openai/deployments/{deployment}/...?api-version=,
	// api-version required, the deployment in the path.
	familyDeployment
	// familyV1: /openai/v1/..., api-version optional (v1 or preview), the
	// deployment in the body's model.
	familyV1
)

// route is what the path of a request says.
type route struct {
	family     family
	deployment string        // familyDeployment only
	endpoint   core.Endpoint // "" when the operation is not served
	operation  string        // the operation name of the 429 text
}

// routeOf parses the path relative to the mount prefix (x-wwz-path).
func routeOf(path string, kind driver.Kind) route {
	if rest, ok := strings.CutPrefix(path, deploymentsPrefix); ok {
		dep, op, _ := strings.Cut(rest, "/")
		r := route{family: familyDeployment, deployment: dep}
		switch op {
		case "chat/completions":
			r.endpoint, r.operation = core.EndpointChat, "ChatCompletions_Create"
		case "completions":
			r.endpoint, r.operation = core.EndpointCompletions, "Completions_Create"
		}
		if dep == "" {
			r.endpoint = ""
		}
		return r
	}
	r := route{}
	if strings.HasPrefix(path, v1Prefix) {
		r.family = familyV1
	}
	switch kind {
	case driver.KindChat:
		r.endpoint, r.operation = core.EndpointChat, "ChatCompletions_Create"
	case driver.KindCompletions:
		r.endpoint, r.operation = core.EndpointCompletions, "Completions_Create"
	case driver.KindResponses:
		r.endpoint, r.operation = core.EndpointResponses, "Responses_Create"
	}
	return r
}

// routeOfHeaders is the route of a request through its headers.
func routeOfHeaders(h driver.Headers, kind driver.Kind) route {
	if h == nil {
		return routeOf("", kind)
	}
	return routeOf(h.Get("x-wwz-path"), kind)
}

// operationOf is the operation name of req for the 429 text.
func operationOf(req *core.Request) string {
	if req != nil && req.Endpoint == core.EndpointCompletions {
		return "Completions_Create"
	}
	if req != nil && req.Endpoint == core.EndpointResponses {
		return "Responses_Create"
	}
	return "ChatCompletions_Create"
}

// apiVersions are the data-plane api-version values the deployment routes
// accept: the GA and preview versions the API version lifecycle page lists
// (spec 006 16.6; GA 2024-10-21 is the latest, 2025-04-01-preview the
// latest preview). Not conferred.
var apiVersions = map[string]bool{
	"2022-12-01": true, "2023-05-15": true, "2024-02-01": true, "2024-06-01": true, "2024-10-21": true,
	"2023-03-15-preview": true, "2023-06-01-preview": true, "2023-07-01-preview": true,
	"2023-08-01-preview": true, "2023-09-01-preview": true, "2023-10-01-preview": true,
	"2023-12-01-preview": true, "2024-02-15-preview": true, "2024-03-01-preview": true,
	"2024-04-01-preview": true, "2024-05-01-preview": true, "2024-07-01-preview": true,
	"2024-08-01-preview": true, "2024-09-01-preview": true, "2024-10-01-preview": true,
	"2024-12-01-preview": true, "2025-01-01-preview": true, "2025-02-01-preview": true,
	"2025-03-01-preview": true, "2025-04-01-preview": true,
}

// apiVersionOK reports whether the api-version of the request is one the
// family accepts: required and known on the deployment routes; absent, v1
// or preview on the v1 API.
func apiVersionOK(r route, h driver.Headers) bool {
	v := driver.Query(h).Get("api-version")
	switch r.family {
	case familyDeployment:
		return apiVersions[v]
	case familyV1:
		return v == "" || v == "v1" || v == "preview"
	}
	return true
}

// apiVersionOf is the api-version the request named, for the 429 text;
// the latest GA when none.
func apiVersionOf(req *core.Request) string {
	if req != nil {
		if v := driver.Query(driver.MapHeaders(req.Headers)).Get("api-version"); v != "" {
			return v
		}
	}
	return "2024-10-21"
}
