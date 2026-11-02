package research

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const ddgHTML = `<html><body>
<div class="result result--ad"><a class="result__a" href="https://ads.example/x">Ad</a></div>
<div class="result results_links"><h2><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&rut=abc">The Go <b>Programming</b> Language</a></h2>
<a class="result__snippet" href="#">Go is an open source programming language.</a></div>
<div class="result"><a class="result__a" href="https://sqlite.org/fts5.html">FTS5</a><div class="result__snippet">Full-text search</div></div>
</body></html>`

const wikiJSON = `{"query":{"search":[{"title":"Go (programming language)","snippet":"<span class=\"searchmatch\">Go</span> is a statically typed language"}]}}`

func testClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/w/api.php":
			if r.URL.Query().Get("srsearch") != "golang" {
				t.Errorf("wiki query = %q", r.URL.RawQuery)
			}
			io.WriteString(w, wikiJSON)
		case r.URL.Path == "/html/":
			io.WriteString(w, ddgHTML)
		case r.URL.Path == "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, `<html><head><title> Test Page </title><script>var x=1</script></head>
				<body><nav>menu</nav><main><h1>Heading</h1><p>First   paragraph.</p><p>Second.</p></main><footer>foot</footer></body></html>`)
		case r.URL.Path == "/bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0, 1})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return New(Options{
		Providers: []string{"wikipedia", "duckduckgo"}, MaxResults: 5, FetchMaxBytes: 64 << 10,
		Timeout: 5 * time.Second, UserAgent: "test", HTTP: srv.Client(),
		WikipediaURL: srv.URL, DuckDuckGoURL: srv.URL + "/html/",
	})
}

func TestSearchMergesProviders(t *testing.T) {
	c := testClient(t)
	res, err := c.Search(context.Background(), "golang")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Source != "wikipedia" || res[0].Snippet != "Go is a statically typed language" ||
		res[0].URL != "https://en.wikipedia.org/wiki/Go_%28programming_language%29" {
		t.Errorf("wiki result = %+v", res[0])
	}
	if res[1].URL != "https://go.dev/doc/" || res[1].Title != "The Go Programming Language" || res[1].Source != "duckduckgo" {
		t.Errorf("ddg result = %+v", res[1])
	}
	for _, r := range res {
		if strings.Contains(r.URL, "ads.example") {
			t.Error("ad result not filtered")
		}
	}
}

func TestFetchExtractsMainText(t *testing.T) {
	c := testClient(t)
	srvURL := c.o.WikipediaURL
	p, err := c.Fetch(context.Background(), srvURL+"/page")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Test Page" || p.Text != "Heading\nFirst paragraph.\nSecond." {
		t.Errorf("page = %+v", p)
	}
	if _, err := c.Fetch(context.Background(), srvURL+"/bin"); err == nil {
		t.Error("binary content accepted")
	}
	if _, err := c.Fetch(context.Background(), "file:///etc/passwd"); err == nil {
		t.Error("file:// accepted")
	}
}

func TestPublicOnlyClientBlocksLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "secret dashboard")
	}))
	defer srv.Close()
	c := New(Options{Providers: []string{"wikipedia"}, Timeout: 2 * time.Second, UserAgent: "t"})
	_, err := c.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "blocked non-public address") {
		t.Fatalf("err = %v, want blocked", err)
	}
}

func TestIsPublicIP(t *testing.T) {
	tests := map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.1.1": false, "100.64.0.1": false, "::1": false, "fe80::1": false, "0.0.0.0": false,
	}
	for ip, want := range tests {
		if got := IsPublicIP(net.ParseIP(ip)); got != want {
			t.Errorf("IsPublicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}
