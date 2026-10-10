package api

import (
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/adfoke/agentshare/internal/store"
)

// The one HTML page in this service: the index a share link opens to when it
// covers more than one file.
//
// It is a template rather than assembled strings for one reason above all
// others — escaping. A file's name comes from whoever uploaded it, and
// html/template escapes it for the context it lands in, so a file called
// `<script>...</script>` is displayed rather than executed. Hand-built HTML
// would have to remember that in every insertion point, forever.
//
//go:embed share_index.html
var shareIndexHTML string

var shareIndexTmpl = template.Must(template.New("share_index").Parse(shareIndexHTML))

// indexFile is one row of the table.
type indexFile struct {
	Name     string
	Href     string
	Modified string
	Size     string
}

type indexPage struct {
	Title     string
	Path      string
	Label     string
	Count     int
	TotalSize string
	Expires   string
	Files     []indexFile
	// Hidden counts members that exist but have expired. Saying so is better
	// than quietly showing a shorter list than the owner expects.
	Hidden   int
	HasJSON  bool
	JSONHref string
}

// renderShareIndex writes the index page for a share.
func (s *Server) renderShareIndex(c *gin.Context, sh *store.Share, files []*store.File, hidden int) {
	base := s.absURL(c, "/s/"+sh.ID)
	page := indexPage{
		Title:    "Index of /s/" + sh.ID,
		Path:     "/s/" + sh.ID + "/",
		Label:    sh.Label,
		Files:    make([]indexFile, 0, len(files)),
		Hidden:   hidden,
		HasJSON:  true,
		JSONHref: base + "/info",
	}
	if sh.ExpiresAt != nil {
		// A share with no expiry says nothing rather than "never", which would
		// be a promise the recipient cannot act on.
		page.Expires = time.Unix(*sh.ExpiresAt, 0).UTC().Format("2006-01-02 15:04") + " UTC"
	}

	var total int64
	for _, f := range files {
		total += f.Size
		page.Files = append(page.Files, indexFile{
			Name:     f.Name,
			Href:     base + "/f/" + f.ID,
			Modified: time.Unix(f.CreatedAt, 0).UTC().Format("02-Jan-2006 15:04"),
			Size:     humanSize(f.Size),
		})
	}
	page.Count = len(files)
	page.TotalSize = humanSize(total)

	c.Header("Content-Type", "text/html; charset=utf-8")
	// The page is generated entirely from this service and carries no script,
	// no external reference and no form. Saying so in a header means a mistake
	// in the template cannot become an execution.
	c.Header("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	c.Header("Cache-Control", "private, no-store")

	c.Status(http.StatusOK)
	if err := shareIndexTmpl.Execute(c.Writer, page); err != nil {
		// The status line is already out by now, so this cannot become an
		// error response — but it must not pass silently either.
		s.log.Error("index template failed", "err", err.Error(), "share", sh.ID, "request_id", requestID(c))
	}
}

// humanSize is the compact form the reference mirror listings use: raw bytes
// with no unit below 1 KiB, otherwise one significant decimal and a suffix.
func humanSize(n int64) string {
	if n < 0 {
		n = 0
	}
	if n < 1024 {
		return strconv.FormatInt(n, 10)
	}
	units := []string{"K", "M", "G", "T", "P"}
	v := float64(n)
	i := -1
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v < 10 {
		return fmt.Sprintf("%.1f%s", v, units[i])
	}
	return fmt.Sprintf("%.0f%s", v, units[i])
}

// shareListingURL is where a share's index lives, used in the JSON replies so a
// client that asked for metadata knows where the human-readable view is.
func shareListingURL(s *Server, c *gin.Context, sh *store.Share) string {
	return s.absURL(c, "/s/"+sh.ID+"/")
}
