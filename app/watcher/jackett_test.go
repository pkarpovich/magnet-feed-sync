package watcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const torznabFixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <item>
      <title>Только на одну ночь / One Night Only (2026) TSRip [H.264] [AD]</title>
      <guid>https://nnmclub.to/forum/download.php?id=1883913</guid>
      <comments>https://nnmclub.to/forum/viewtopic.php?t=1883913</comments>
      <link>http://172.18.0.9:9117/dl/nnmclub/?jackett_apikey=k&amp;path=abc&amp;file=one.torrent</link>
      <pubDate>Mon, 04 Aug 2026 12:00:00 +0000</pubDate>
      <enclosure url="http://172.18.0.9:9117/dl/nnmclub/?path=abc" type="application/x-bittorrent" />
      <torznab:attr name="seeders" value="12" />
      <torznab:attr name="peers" value="15" />
    </item>
    <item>
      <title>One.Night.Only.2026.1080p.WEB-DL.DDP5.1.H264-GROUP</title>
      <guid>https://rutracker.org/forum/viewtopic.php?t=6543210</guid>
      <comments></comments>
      <link></link>
      <enclosure url="http://172.18.0.9:9117/dl/rutracker/?path=def" type="application/x-bittorrent" />
      <pubDate>Tue, 05 Aug 2026 08:30:00 +0000</pubDate>
      <torznab:attr name="seeders" value="3" />
    </item>
  </channel>
</rss>`

func newFixtureServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.WriteHeader(status)
		_, err := w.Write([]byte(body))
		require.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestJackettSourceSearchMapsEveryItem(t *testing.T) {
	srv := newFixtureServer(t, torznabFixture, http.StatusOK)
	source := NewJackettSource(JackettOptions{BaseURL: srv.URL, APIKey: "secret"})

	results, err := source.Search(context.Background(), "One Night Only 2026")
	require.NoError(t, err)
	require.Len(t, results, 2)

	first := results[0]
	assert.Equal(t, "jackett", first.Source)
	assert.Equal(t, "1883913", first.ExternalID)
	assert.Equal(t, "Только на одну ночь / One Night Only (2026) TSRip [H.264] [AD]", first.Title)
	assert.Equal(t, "https://nnmclub.to/forum/viewtopic.php?t=1883913", first.PageURL)
	assert.Equal(t, "One Night Only 2026", first.Query)
	assert.Equal(t, 12, first.Seeders)
	assert.Equal(t, time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC), first.PublishedAt.UTC())

	second := results[1]
	assert.Equal(t, "6543210", second.ExternalID)
	assert.Equal(t, "https://rutracker.org/forum/viewtopic.php?t=6543210", second.PageURL)
	assert.Equal(t, 3, second.Seeders)
	assert.Contains(t, second.DownloadURL, "/dl/rutracker/", "falls back to <enclosure> when <link> is empty")
}

func TestJackettSourceSearchSendsTheTorznabRequest(t *testing.T) {
	var gotPath, gotQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, err := w.Write([]byte(torznabFixture))
		require.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	source := NewJackettSource(JackettOptions{BaseURL: srv.URL + "/api/v2.0/indexers/all/results", APIKey: "secret"})

	_, err := source.Search(context.Background(), "One Night Only 2026")
	require.NoError(t, err)

	assert.Equal(t, jackettSearchPath, gotPath)
	assert.Equal(t, "apikey=secret&q=One+Night+Only+2026&t=search", gotQuery)
}

func TestJackettSourceSearchRewritesTheDownloadHost(t *testing.T) {
	srv := newFixtureServer(t, torznabFixture, http.StatusOK)
	source := NewJackettSource(JackettOptions{
		BaseURL:   srv.URL,
		PublicURL: "https://jackett.example.com",
		APIKey:    "secret",
	})

	results, err := source.Search(context.Background(), "One Night Only 2026")
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Equal(t,
		"https://jackett.example.com/dl/nnmclub/?jackett_apikey=k&path=abc&file=one.torrent",
		results[0].DownloadURL,
	)
	assert.Equal(t, "https://jackett.example.com/dl/rutracker/?path=def", results[1].DownloadURL)
}

func TestJackettSourceSearchDefaultsThePublicHostToTheBase(t *testing.T) {
	srv := newFixtureServer(t, torznabFixture, http.StatusOK)
	source := NewJackettSource(JackettOptions{BaseURL: srv.URL, APIKey: "secret"})

	results, err := source.Search(context.Background(), "One Night Only 2026")
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Equal(t, srv.URL+"/dl/nnmclub/?jackett_apikey=k&path=abc&file=one.torrent", results[0].DownloadURL)
}

func TestJackettSourceSearchFailsOnNonSuccessStatus(t *testing.T) {
	srv := newFixtureServer(t, "denied", http.StatusUnauthorized)
	source := NewJackettSource(JackettOptions{BaseURL: srv.URL, APIKey: "secret"})

	results, err := source.Search(context.Background(), "One Night Only 2026")
	require.Error(t, err)
	assert.Nil(t, results)
	assert.Contains(t, err.Error(), "401")
}

func TestJackettSourceSearchFailsOnMalformedXML(t *testing.T) {
	srv := newFixtureServer(t, "<rss><channel><item></channel>", http.StatusOK)
	source := NewJackettSource(JackettOptions{BaseURL: srv.URL, APIKey: "secret"})

	results, err := source.Search(context.Background(), "One Night Only 2026")
	require.Error(t, err)
	assert.Nil(t, results)
	assert.Contains(t, err.Error(), "parse response")
}

func TestJackettSourceSearchSkipsItemWithoutIdentity(t *testing.T) {
	const fixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <item><title>No identity at all</title></item>
    <item>
      <title>One.Night.Only.2026.1080p.WEB-DL</title>
      <comments>https://nnmclub.to/forum/viewtopic.php?t=1883913</comments>
    </item>
  </channel>
</rss>`

	srv := newFixtureServer(t, fixture, http.StatusOK)
	source := NewJackettSource(JackettOptions{BaseURL: srv.URL, APIKey: "secret"})

	results, err := source.Search(context.Background(), "One Night Only 2026")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "1883913", results[0].ExternalID)
}

func TestJackettSourceSearchFallsBackToTheRawGUID(t *testing.T) {
	const fixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <item>
      <title>One.Night.Only.2026.1080p.WEB-DL</title>
      <guid>abcdef0123456789</guid>
    </item>
  </channel>
</rss>`

	srv := newFixtureServer(t, fixture, http.StatusOK)
	source := NewJackettSource(JackettOptions{BaseURL: srv.URL, APIKey: "secret"})

	results, err := source.Search(context.Background(), "One Night Only 2026")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "abcdef0123456789", results[0].ExternalID)
	assert.Empty(t, results[0].PageURL)
	assert.True(t, results[0].PublishedAt.IsZero())
}

func TestJackettSourceSearchFailsWithoutConfiguration(t *testing.T) {
	noKey := NewJackettSource(JackettOptions{BaseURL: "http://jackett:9117"})
	_, err := noKey.Search(context.Background(), "One Night Only 2026")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api key")

	noBase := NewJackettSource(JackettOptions{APIKey: "secret"})
	_, err = noBase.Search(context.Background(), "One Night Only 2026")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base url")
}

func TestJackettSourceName(t *testing.T) {
	assert.Equal(t, "jackett", NewJackettSource(JackettOptions{}).Name())
}

var _ SearchSource = (*JackettSource)(nil)
