package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 页面可服务且为完整 HTML（含标题与基础结构）
func TestHandlerServesIndex(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type: %q", ct)
	}

	buf := make([]byte, len(indexHTML))
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	for _, want := range []string{"light-github 控制台", `id="tab-dash"`, `id="tab-settings"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("页面缺少 %s", want)
		}
	}
}
