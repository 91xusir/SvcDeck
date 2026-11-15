package mcpserver

import (
	"io"
	"net/http"
	"strings"
)

// NewHTTPHandler 返回 Streamable HTTP MCP 端点。
// 客户端把 JSON-RPC POST 到同一个 URL；notification 返回 202。
// 服务端不主动推送，因此 GET/DELETE 返回 405（MCP 允许这样降级）。
func NewHTTPHandler(cfg Config) (http.Handler, error) {
	s, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return &httpHandler{srv: s}, nil
}

type httpHandler struct {
	srv *Server
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "MCP endpoint only accepts POST", http.StatusMethodNotAllowed)
		return
	}

	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	resp, ok, err := h.srv.HandleJSON(r.Context(), body)
	if err != nil {
		http.Error(w, "internal MCP error", http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}
