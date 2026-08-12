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

type solver interface {
	Solve(ctx context.Context, url string) (*providers.SolvedPage, error)
}

type ExttoOptions struct {
	BaseURL string
	Solver  solver
}

type ExttoSource struct {
	baseURL string
	solver  solver
	client  *http.Client

	mu        sync.Mutex
	cookie    string
	userAgent string
	pageToken string
	csrfToken string
	// the signed magnet POST has to reproduce both
	tokenQuery   string
	tokenSession exttoSession
}

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

	body, session, err := s.browse(ctx, query)
	if err != nil {
		return nil, err
	}

	return s.parse(body, query, session)
}

// query is required: the signature uses tokens from that query's page. The nil receiver is
// handled, not dereferenced — a typed nil in an interface passes the caller's != nil check
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

	// sent under the cookie the tokens were minted for: a concurrent refresh would pair this
	// query's tokens with another session, which ext.to refuses
	body, err := s.postForm(ctx, s.baseURL+exttoMagnetPath, form, tokens.session)
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

func (s *ExttoSource) browse(ctx context.Context, query string) ([]byte, exttoSession, error) {
	if s.session().cookie == "" {
		if err := s.refreshCookie(ctx); err != nil {
			return nil, exttoSession{}, err
		}
	}

	endpoint := s.searchURL(query)
	body, session, err := s.get(ctx, endpoint)
	if err != nil {
		return nil, exttoSession{}, err
	}
	if !isExttoChallenge(body) {
		return body, session, nil
	}

	// only a GET may be replayed after a refresh; a signed POST must be re-issued by its caller
	if err := s.refreshCookie(ctx); err != nil {
		return nil, exttoSession{}, err
	}
	body, session, err = s.get(ctx, endpoint)
	if err != nil {
		return nil, exttoSession{}, err
	}
	if isExttoChallenge(body) {
		return nil, exttoSession{}, errors.New("search extto: cloudflare challenge survived a cookie refresh")
	}

	return body, session, nil
}

func (s *ExttoSource) searchURL(query string) string {
	params := url.Values{}
	params.Set("q", query)
	params.Set("sort", exttoSort)
	params.Set("order", exttoOrder)

	return s.baseURL + exttoBrowsePath + "?" + params.Encode()
}

func (s *ExttoSource) get(ctx context.Context, endpoint string) ([]byte, exttoSession, error) {
	session := s.session()

	req, err := s.newRequest(ctx, http.MethodGet, endpoint, nil, session)
	if err != nil {
		return nil, exttoSession{}, err
	}

	body, status, err := s.do(req)
	if err != nil {
		return nil, exttoSession{}, err
	}

	// a challenge must reach the caller as a body, not a status error, so it can refresh the cookie
	if (status < 200 || status >= 300) && !isExttoChallenge(body) {
		return nil, exttoSession{}, fmt.Errorf("search extto: unexpected status %d", status)
	}

	return body, session, nil
}

func (s *ExttoSource) postForm(ctx context.Context, endpoint string, form url.Values, session exttoSession) ([]byte, error) {
	req, err := s.newRequest(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()), session)
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

func (s *ExttoSource) newRequest(
	ctx context.Context, method, endpoint string, body io.Reader, session exttoSession,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("build extto request: %w", err)
	}

	req.Header.Set("User-Agent", session.userAgent)
	req.Header.Set("Cookie", session.cookie)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Referer", s.baseURL+"/")

	return req, nil
}

func (s *ExttoSource) do(req *http.Request) ([]byte, int, error) {
	res, err := s.client.Do(req)
	if err != nil {
		// the *url.Error wrapper would carry the full request url into last_status and loki
		return nil, 0, fmt.Errorf("call extto: %w", providers.WithoutURL(err))
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

// hmac uses the page token, sessid the csrf token; ext.to checks both against the fetching cookie
type exttoTokens struct {
	page    string
	csrf    string
	session exttoSession
}

func (s *ExttoSource) tokensFor(ctx context.Context, query string) (exttoTokens, error) {
	tokens, ok, searched := s.heldTokens(query)
	if ok {
		return tokens, nil
	}
	// page already fetched this session with no usable tokens: replaying would run a full
	// challenge-fenced search per row
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

func (s *ExttoSource) heldTokens(query string) (exttoTokens, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	searched := s.tokenQuery == query
	if !searched || s.pageToken == "" || s.csrfToken == "" {
		return exttoTokens{}, false, searched
	}

	return exttoTokens{page: s.pageToken, csrf: s.csrfToken, session: s.tokenSession}, true, true
}

func (s *ExttoSource) storeTokens(doc *goquery.Document, query string, session exttoSession) {
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
	s.tokenSession = session
}

func (s *ExttoSource) parse(body []byte, query string, session exttoSession) ([]SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("search extto: parse response: %w", err)
	}
	s.storeTokens(doc, query, session)

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
		// query terms come back wrapped in highlight tags
		Title:       strings.TrimSpace(link.Text()),
		PageURL:     pageURL,
		Query:       query,
		Seeders:     s.seeders(row),
		PublishedAt: s.publishedAt(row),
	}, true
}

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

// the cell text is a relative age ("1 year ago"); only the title attribute carries a date
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

// the status code is not a reliable signal, and a solved page still mentions challenge-platform
var exttoChallengeMarkers = [][]byte{[]byte("Just a moment"), []byte("_cf_chl_opt")}

func isExttoChallenge(body []byte) bool {
	for _, marker := range exttoChallengeMarkers {
		if bytes.Contains(body, marker) {
			return true
		}
	}

	return false
}
