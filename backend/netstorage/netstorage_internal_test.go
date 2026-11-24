package netstorage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiTestHeaders is a deliberately literal copy of apiHeaders, without
// the "*" prefix, so that a header dropped from the production list
// shows up as a leak in the cross host tests rather than silently
// losing coverage.
var apiTestHeaders = []string{"X-Akamai-ACS-Auth-Data", "X-Akamai-ACS-Auth-Sign", "X-Akamai-ACS-Action"}

// statXML is a minimal stat response saying the root is a directory
const statXML = `<?xml version="1.0" encoding="UTF-8"?>
<stat directory="/dir"><file type="dir" name="dir" mtime="1"/></stat>`

// newTestFs makes an Fs via NewFs pointing at serverURL.
//
// NewFs makes a stat request for the root and treats any failure as
// the root not existing, so callers check what the servers received
// or make a further request rather than relying on its error.
func newTestFs(ctx context.Context, t *testing.T, serverURL string) *Fs {
	u, err := url.Parse(serverURL)
	require.NoError(t, err)
	m := configmap.Simple{
		"type":     "netstorage",
		"protocol": u.Scheme,
		"host":     u.Host + "/dir",
		"account":  "test-account",
		"secret":   obscure.MustObscure("test-secret"),
	}
	f, err := NewFs(ctx, "netstorage-test", "", m)
	require.NoError(t, err)
	return f.(*Fs)
}

// assertAPIHeaders checks all the API headers are present (want) or
// absent (!want) in r.
func assertAPIHeaders(t *testing.T, r *http.Request, want bool) {
	for _, header := range apiTestHeaders {
		if want {
			assert.NotEmpty(t, r.Header.Get(header), "%s should be sent", header)
		} else {
			assert.Empty(t, r.Header.Get(header), "%s should have been stripped", header)
		}
	}
}

// TestRedirectStripsAPIHeadersOnCrossHost checks the API headers are
// sent to the configured host but not to another host it redirects to.
func TestRedirectStripsAPIHeadersOnCrossHost(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		assertAPIHeaders(t, r, false)
		_, _ = w.Write([]byte(statXML))
	}))
	defer other.Close()

	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		assertAPIHeaders(t, r, true)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	newTestFs(context.Background(), t, origin.URL)
	assert.Equal(t, int32(1), originHits.Load())
	assert.Equal(t, int32(1), otherHits.Load())
}

// TestUploadStripsAPIHeadersOnCrossHost checks an upload, which
// carries the API headers on the PUT as well as in the trailers, does
// not send them to another host the configured host redirects to.
func TestUploadStripsAPIHeadersOnCrossHost(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		assertAPIHeaders(t, r, false)
		_, _ = w.Write([]byte(statXML))
	}))
	defer other.Close()

	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		assertAPIHeaders(t, r, true)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer origin.Close()

	ctx := context.Background()
	f := newTestFs(ctx, t, origin.URL)
	o := &Object{fs: f, remote: "file"}
	src := object.NewStaticObjectInfo("file", time.Now(), 4, true, nil, f)
	err := o.netStorageUploadRequest(ctx, strings.NewReader("data"), src)
	require.NoError(t, err)
	// NewFs stat, PUT and the stat after the upload
	assert.Equal(t, int32(3), originHits.Load())
	assert.Equal(t, int32(3), otherHits.Load())
}

// TestRedirectKeepsAPIHeadersOnSameHost checks the API headers are
// still sent after a redirect which stays on the configured host.
func TestRedirectKeepsAPIHeadersOnSameHost(t *testing.T) {
	var hits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		assertAPIHeaders(t, r, true)
		if !strings.HasSuffix(r.URL.Path, "/moved") {
			http.Redirect(w, r, r.URL.Path+"/moved", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte(statXML))
	}))
	defer origin.Close()

	newTestFs(context.Background(), t, origin.URL)
	assert.Equal(t, int32(2), hits.Load())
}

// TestRedirectRefusesHTTPSDowngrade checks a redirect from HTTPS to
// plaintext HTTP is not followed.
func TestRedirectRefusesHTTPSDowngrade(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(statXML))
	}))
	defer target.Close()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	ctx, ci := fs.AddConfig(context.Background())
	ci.InsecureSkipVerify = true
	f := newTestFs(ctx, t, origin.URL)
	_, err := f.netStorageStatRequest(ctx, f.url(""), true)
	require.ErrorIs(t, err, rest.ErrHTTPSDowngrade)
	assert.Equal(t, int32(0), targetHits.Load())
}
