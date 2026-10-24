package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
)

// toolRoute is an endpoint served by the MCP tool of the same name, called
// in-process: the screen gets exactly what Claude gets.
func (srv *Server) toolRoute(pattern, tool string) apiRoute {
	return apiRoute{pattern, tool, srv.tool(tool)}
}

// tool calls an MCP tool with the request's arguments: a GET's query
// string (converted to the tool's argument types) or a POST's JSON body.
func (srv *Server) tool(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h, ok := srv.Tools[name]
		if !ok {
			apiFail(w, badRequest("This needs the %s tool, which this process does not offer.", name))
			return
		}
		var args json.RawMessage
		if r.Method == http.MethodGet {
			b, err := queryArgs(r.URL.Query(), h.In)
			if err != nil {
				apiFail(w, badRequest("%v", err))
				return
			}
			args = b
		} else {
			b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
			if err != nil {
				apiFail(w, badRequest("The request is too large or unreadable."))
				return
			}
			args = b
		}
		out, err := h.Call(r.Context(), args)
		if err != nil {
			toolFail(w, name, err)
			return
		}
		reply(w, http.StatusOK, out)
	}
}

// toolFail answers a tool's refusal. Tools explain what was wrong in their
// error, so the message is shown as it is.
func toolFail(w http.ResponseWriter, name string, err error) {
	switch {
	case errors.Is(err, outreach.ErrStale):
		reply(w, http.StatusConflict, errBody(strings.TrimPrefix(err.Error(), outreach.ErrStale.Error()+": ")))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		reply(w, http.StatusServiceUnavailable, errBody("The request took too long; try again."))
	default:
		slog.Info("api: tool refused", "tool", name, "err", err)
		reply(w, http.StatusBadRequest, errBody(strings.TrimPrefix(err.Error(), outreach.ErrInvalid.Error()+": ")))
	}
}

// queryArgs turns a query string into the JSON object a tool's argument
// struct expects, converting each value to its field's type.
func queryArgs(q url.Values, in reflect.Type) (json.RawMessage, error) {
	for in.Kind() == reflect.Pointer {
		in = in.Elem()
	}
	fields := map[string]reflect.Type{}
	if in.Kind() == reflect.Struct {
		for i := range in.NumField() {
			f := in.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name != "" && name != "-" {
				fields[name] = f.Type
			}
		}
	}
	args := map[string]any{}
	for key, vals := range q {
		t, ok := fields[key]
		if !ok {
			return nil, fmt.Errorf("unknown parameter %q", key)
		}
		v, err := convert(vals, t)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", key, err)
		}
		args[key] = v
	}
	return json.Marshal(args)
}

func convert(vals []string, t reflect.Type) (any, error) {
	if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
		var out []any
		for _, v := range vals {
			for _, part := range strings.Split(v, ",") {
				x, err := convert([]string{part}, t.Elem())
				if err != nil {
					return nil, err
				}
				out = append(out, x)
			}
		}
		return out, nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	v := vals[len(vals)-1]
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint32, reflect.Uint64:
		return strconv.ParseInt(v, 10, 64)
	case reflect.Float32, reflect.Float64:
		return strconv.ParseFloat(v, 64)
	case reflect.Bool:
		return strconv.ParseBool(v)
	case reflect.String:
		return v, nil
	}
	return nil, fmt.Errorf("cannot be given in a URL")
}
