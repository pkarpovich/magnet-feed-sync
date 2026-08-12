package watcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"magnet-feed-sync/app/tracker/providers"
)

const (
	exttoBaseURL       = "https://search.extto.com"
	exttoBrowsePath    = "/browse/"
	exttoMagnetPath    = "/ajax/getSearchMagnet.php"
	exttoSort          = "size"
	exttoOrder         = "desc"
	exttoSearchTimeout = 180 * time.Second
	exttoDateLayout    = "02 Jan 2006"
)

// solver is the consumer-side view of the FlareSolverr client. The ext.to source calls it
// only to refresh the cookie; every other request goes through its own http client.
type solver interface {
	Solve(ctx context.Context, url string) (*providers.SolvedPage, error)
}

// ExttoOptions configures the source. An empty BaseURL falls back to the public site; a nil
// Solver leaves the source disabled, reporting a configuration error rather than no rows.
type ExttoOptions struct {
	BaseURL string
	Solver  solver
}

// ExttoSource searches search.extto.com, which sits behind a Cloudflare challenge. The
// cookie, the User-Agent and the two page tokens are process-wide state shared by the cron
// cycle and the http handlers, so every access is guarded.
type ExttoSource struct {
	baseURL string
	solver  solver
	client  *http.Client

	mu         sync.Mutex
	cookie     string
	userAgent  string
	pageToken  string
	csrfToken  string
	tokenQuery string
}

// NewExttoSource builds an ext.to source. It never dials anything at construction time.
func NewExttoSource(o ExttoOptions) *ExttoSource {
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = exttoBaseURL
	}

	return &ExttoSource{
		baseURL: base,
		solver:  o.Solver,
		client:  &http.Client{Timeout: exttoSearchTimeout},
	}
}

func (s *ExttoSource) Name() string {
	return SourceExtto
}

func (s *ExttoSource) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if s == nil || s.solver == nil {
		return nil, errors.New("search extto: flaresolverr is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, exttoSearchTimeout)
	defer cancel()

	body, err := s.browse(ctx, query)
	if err != nil {
		return nil, err
	}

	return s.parse(body, query)
}

// Magnet resolves the magnet link of one row. query is required because the signature is
// built from tokens carried by that query's search page, so the search is replayed when no
// fresh tokens are held.
// The nil receiver is handled rather than dereferenced: a typed nil in an interface field
// passes the caller's `!= nil` check, and the composition root is then the only thing
// standing between a disabled ext.to source and a panic on the first magnet call.
func (s *ExttoSource) Magnet(ctx context.Context, torrentID, query string) (string, error) {
	if s == nil || s.solver == nil {
		return "", errors.New("extto magnet: flaresolverr is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, exttoSearchTimeout)
	defer cancel()

	tokens, err := s.tokensFor(ctx, query)
	if err != nil {
		return "", err
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	form := url.Values{}
	form.Set("torrent_id", torrentID)
	form.Set("hash", "")
	form.Set("name", "")
	form.Set("timestamp", ts)
	form.Set("hmac", s.sign(torrentID, ts, tokens.page))
	form.Set("sessid", tokens.csrf)

	body, err := s.postForm(ctx, s.baseURL+exttoMagnetPath, form)
	if err != nil {
		return "", err
	}

	var decoded struct {
		Success bool   `json:"success"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("extto magnet: decode response: %w", err)
	}
	if !decoded.Success || decoded.URL == "" {
		return "", fmt.Errorf("extto magnet: refused for torrent %s", torrentID)
	}

	return decoded.URL, nil
}

func (s *ExttoSource) sign(torrentID, ts, pageToken string) string {
	digest := sha256.Sum256([]byte(torrentID + "|" + ts + "|" + pageToken))

	return hex.EncodeToString(digest[:])
}

func (s *ExttoSource) browse(ctx context.Context, query string) ([]byte, error) {
	if s.session().cookie == "" {
		if err := s.refreshCookie(ctx); err != nil {
			return nil, err
		}
	}

	endpoint := s.searchURL(query)
	body, err := s.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if !isExttoChallenge(body) {
		return body, nil
	}

	// only a GET may be replayed after a refresh; a signed POST carries tokens of its own
	// and has to be re-issued by its caller
	if err := s.refreshCookie(ctx); err != nil {
		return nil, err
	}
	body, err = s.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if isExttoChallenge(body) {
		return nil, errors.New("search extto: cloudflare challenge survived a cookie refresh")
	}

	return body, nil
}

func (s *ExttoSource) searchURL(query string) string {
	params := url.Values{}
	params.Set("q", query)
	params.Set("sort", exttoSort)
	params.Set("order", exttoOrder)

	return s.baseURL + exttoBrowsePath + "?" + params.Encode()
}

func (s *ExttoSource) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := s.newRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	body, status, err := s.do(req)
	if err != nil {
		return nil, err
	}

	// a challenge answers with a status of its own, and the caller handles it by refreshing
	// the cookie, so it must reach them as a body rather than as a status error
	if (status < 200 || status >= 300) && !isExttoChallenge(body) {
		return nil, fmt.Errorf("search extto: unexpected status %d", status)
	}

	return body, nil
}

func (s *ExttoSource) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := s.newRequest(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Origin", s.baseURL)

	body, status, err := s.do(req)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("extto magnet: unexpected status %d", status)
	}

	return body, nil
}

func (s *ExttoSource) newRequest(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("build extto request: %w", err)
	}

	session := s.session()
	req.Header.Set("User-Agent", session.userAgent)
	req.Header.Set("Cookie", session.cookie)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Referer", s.baseURL+"/")

	return req, nil
}

func (s *ExttoSource) do(req *http.Request) ([]byte, int, error) {
	res, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("call extto: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Warn("failed to close extto response body", "error", err)
		}
	}()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxSearchResponseSize))
	if err != nil {
		return nil, res.StatusCode, fmt.Errorf("call extto: read response: %w", err)
	}

	return body, res.StatusCode, nil
}

func (s *ExttoSource) refreshCookie(ctx context.Context) error {
	if s.solver == nil {
		return errors.New("refresh extto cookie: flaresolverr is not configured")
	}

	page, err := s.solver.Solve(ctx, s.baseURL+"/")
	if err != nil {
		return fmt.Errorf("refresh extto cookie: %w", err)
	}
	if page == nil || len(page.Cookies) == 0 {
		return errors.New("refresh extto cookie: solver returned no cookies")
	}

	pairs := make([]string, 0, len(page.Cookies))
	for _, c := range page.Cookies {
		pairs = append(pairs, c.Name+"="+c.Value)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cookie = strings.Join(pairs, "; ")
	s.userAgent = page.UserAgent

	return nil
}

type exttoSession struct {
	cookie    string
	userAgent string
}

func (s *ExttoSource) session() exttoSession {
	s.mu.Lock()
	defer s.mu.Unlock()

	return exttoSession{cookie: s.cookie, userAgent: s.userAgent}
}

type exttoTokens struct {
	page string
	csrf string
}

// tokensFor returns the tokens of query's search page, running the search itself when the
// held ones belong to another query or none are held at all.
func (s *ExttoSource) tokensFor(ctx context.Context, query string) (exttoTokens, error) {
	tokens, ok, searched := s.heldTokens(query)
	if ok {
		return tokens, nil
	}
	// the query's page has already been fetched this session and carried no usable tokens —
	// markup ext.to changed under us. Replaying the search would do it once per row, and a
	// search is a full challenge-fenced round trip holding the shared solver.
	if searched {
		return exttoTokens{}, fmt.Errorf("extto magnet: no page tokens for query %q", query)
	}

	if _, err := s.Search(ctx, query); err != nil {
		return exttoTokens{}, err
	}

	tokens, ok, _ = s.heldTokens(query)
	if !ok {
		return exttoTokens{}, fmt.Errorf("extto magnet: no page tokens for query %q", query)
	}

	return tokens, nil
}

// heldTokens reports the tokens of query's page, whether they are usable, and whether that
// page was fetched at all — the last one is what stops a token-less page from being
// re-searched once per row.
func (s *ExttoSource) heldTokens(query string) (exttoTokens, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	searched := s.tokenQuery == query
	if !searched || s.pageToken == "" || s.csrfToken == "" {
		return exttoTokens{}, false, searched
	}

	return exttoTokens{page: s.pageToken, csrf: s.csrfToken}, true, true
}

func (s *ExttoSource) storeTokens(doc *goquery.Document, query string) {
	page := ""
	if match := searchPageTokenPattern.FindStringSubmatch(doc.Text()); len(match) == 2 {
		page = match[1]
	}
	csrf, _ := doc.Find(`meta[name="csrf-token"]`).First().Attr("content")

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageToken = page
	s.csrfToken = csrf
	s.tokenQuery = query
}

func (s *ExttoSource) parse(body []byte, query string) ([]SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("search extto: parse response: %w", err)
	}
	s.storeTokens(doc, query)

	var results []SearchResult
	doc.Find("tbody tr").Each(func(_ int, row *goquery.Selection) {
		result, ok := s.rowResult(row, query)
		if !ok {
			return
		}
		results = append(results, result)
	})

	return results, nil
}

func (s *ExttoSource) rowResult(row *goquery.Selection, query string) (SearchResult, bool) {
	button := row.Find("a.search-magnet-btn").First()
	if button.Length() == 0 {
		return SearchResult{}, false
	}

	externalID, _ := button.Attr("data-id")
	if externalID == "" {
		slog.Warn("skipping extto row without an id", "query", query)
		return SearchResult{}, false
	}

	link := row.Find("a.torrent-title-link").First()
	pageURL := ""
	if href, ok := link.Attr("href"); ok && href != "" {
		pageURL = s.baseURL + href
	}

	return SearchResult{
		Source:     SourceExtto,
		ExternalID: externalID,
		// the query terms come back wrapped in highlight tags, so only the text is the title
		Title:       strings.TrimSpace(link.Text()),
		PageURL:     pageURL,
		Query:       query,
		Seeders:     s.seeders(row),
		PublishedAt: s.publishedAt(row),
	}, true
}

// labelledValue returns the value span of the cell carrying the given label. The numeric
// cells are keyed by their label rather than by position, so a column reorder degrades to a
// missing field instead of a wrong one.
func (s *ExttoSource) labelledValue(row *goquery.Selection, label string) *goquery.Selection {
	var value *goquery.Selection
	row.Find("div.add-block-wrapper").EachWithBreak(func(_ int, cell *goquery.Selection) bool {
		if !strings.EqualFold(strings.TrimSpace(cell.Find("span.add-block").First().Text()), label) {
			return true
		}
		value = cell.Find("span").Not(".add-block").First()

		return false
	})

	return value
}

func (s *ExttoSource) seeders(row *goquery.Selection) int {
	value := s.labelledValue(row, "Seeds")
	if value == nil || value.Length() == 0 {
		return 0
	}

	seeders, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(value.Text()), ",", ""))
	if err != nil {
		return 0
	}

	return seeders
}

// publishedAt reads the Age cell's title attribute: its text is a relative age ("1 year
// ago") and carries no date.
func (s *ExttoSource) publishedAt(row *goquery.Selection) time.Time {
	value := s.labelledValue(row, "Age")
	if value == nil || value.Length() == 0 {
		return time.Time{}
	}

	title, ok := value.Attr("title")
	if !ok {
		return time.Time{}
	}

	published, err := time.Parse(exttoDateLayout, strings.TrimSpace(title))
	if err != nil {
		return time.Time{}
	}

	return published
}

var searchPageTokenPattern = regexp.MustCompile(`searchPageToken\s*=\s*['"]([0-9a-f]{32})['"]`)

// exttoChallengeMarkers are what a Cloudflare interstitial carries. The status code is not
// a reliable signal, and a solved page still mentions challenge-platform.
var exttoChallengeMarkers = [][]byte{[]byte("Just a moment"), []byte("_cf_chl_opt")}

func isExttoChallenge(body []byte) bool {
	for _, marker := range exttoChallengeMarkers {
		if bytes.Contains(body, marker) {
			return true
		}
	}

	return false
}
