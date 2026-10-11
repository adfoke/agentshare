package api

import (
	"encoding/json"

	"github.com/gin-gonic/gin"

	"github.com/adfoke/agentshare/internal/store"
)

// Every response carries absolute URLs, not just ids: an agent that gets a
// response should be able to act on it without assembling paths itself.

type fileURLs struct {
	Self     string `json:"self"`
	Content  string `json:"content"`
	Download string `json:"download"`
	ByHash   string `json:"by_hash"`
	Public   string `json:"public,omitempty"`
}

type fileDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Sha256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	// Kind is what the bytes are, classified from their magic bytes; Executable
	// and Scriptable spell out the two halves of that answer that matter to a
	// caller about to run something. See kind.go.
	Kind       string          `json:"kind,omitempty"`
	Executable bool            `json:"executable"`
	Scriptable bool            `json:"scriptable"`
	Tags       []string        `json:"tags,omitempty"`
	Meta       json.RawMessage `json:"meta,omitempty"`
	Owner      string          `json:"owner,omitempty"`
	CreatedAt  string          `json:"created_at"`
	ExpiresAt  *string         `json:"expires_at"`
	Downloads  int64           `json:"downloads"`
	URLs       fileURLs        `json:"urls"`
}

func (s *Server) fileDTO(c *gin.Context, f *store.File) fileDTO {
	// Classifying costs one read of the first 512 bytes of the blob, paid on
	// every description — including each row of a listing. A listing is bounded
	// at 1000 rows and the read lands in the page cache, so the price is a few
	// milliseconds; in exchange, "which of these would run if I ran it" becomes
	// a question the API answers without anyone downloading the bytes.
	kind := s.blobKind(f.Sha256)
	dto := fileDTO{
		ID:          f.ID,
		Name:        f.Name,
		Size:        f.Size,
		Sha256:      f.Sha256,
		ContentType: f.ContentType,
		Kind:        kind.Kind,
		Executable:  kind.Executable,
		Scriptable:  kind.Scriptable,
		Tags:        f.Tags,
		Owner:       f.Owner,
		CreatedAt:   timeStr(f.CreatedAt),
		ExpiresAt:   timePtrStr(f.ExpiresAt),
		Downloads:   f.Downloads,
		URLs: fileURLs{
			Self:     s.absURL(c, "/v1/files/"+f.ID),
			Content:  s.absURL(c, "/v1/files/"+f.ID+"/content"),
			Download: s.absURL(c, "/v1/files/"+f.ID+"/content?download=1"),
			ByHash:   s.absURL(c, "/v1/content/"+f.Sha256),
		},
	}
	if f.Meta != "" && f.Meta != "{}" && json.Valid([]byte(f.Meta)) {
		dto.Meta = json.RawMessage(f.Meta)
	}
	return dto
}

// shareItemDTO names one file inside a share. It is deliberately thinner than a
// full fileDTO: a share covering a few hundred files should not repeat every
// digest and URL of each one in its own description.
type shareItemDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

type shareDTO struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
	Owner string `json:"owner,omitempty"`
	// IndexURL is where a browser sees the listing. For a share of several
	// files it is the same as URL; it is spelled out so a client never has to
	// know which of the two shapes it is holding.
	IndexURL     string         `json:"index_url"`
	FileCount    int            `json:"file_count"`
	FileIDs      []string       `json:"file_ids,omitempty"`
	Files        []shareItemDTO `json:"files,omitempty"`
	CreatedAt    string         `json:"created_at"`
	ExpiresAt    *string        `json:"expires_at"`
	MaxDownloads *int64         `json:"max_downloads"`
	Downloads    int64          `json:"downloads"`
	// File is filled in only for a share of exactly one file, which is where the
	// original single-file shape still applies and existing clients read it.
	File *fileDTO `json:"file,omitempty"`
}

func (s *Server) shareDTO(c *gin.Context, sh *store.Share, files ...*store.File) shareDTO {
	base := s.absURL(c, "/s/"+sh.ID)

	// A share always knows how many files it covers, but the members are only
	// passed in by the calls that had reason to load them. Listing many shares
	// should not drag every member of each one along, so the count has to come
	// from the share when the slice is empty — otherwise the listing reports
	// that every share covers nothing.
	count := len(files)
	if count == 0 {
		count = sh.ItemCount
	}

	dto := shareDTO{
		ID:           sh.ID,
		URL:          base,
		Label:        sh.Label,
		Owner:        sh.Owner,
		IndexURL:     base + "/",
		FileCount:    count,
		CreatedAt:    timeStr(sh.CreatedAt),
		ExpiresAt:    timePtrStr(sh.ExpiresAt),
		MaxDownloads: sh.MaxDownloads,
		Downloads:    sh.Downloads,
	}
	for _, f := range files {
		dto.FileIDs = append(dto.FileIDs, f.ID)
		dto.Files = append(dto.Files, shareItemDTO{
			ID:   f.ID,
			Name: f.Name,
			Size: f.Size,
			URL:  base + "/f/" + f.ID,
		})
	}
	if len(files) == 1 {
		fd := s.fileDTO(c, files[0])
		fd.URLs.Public = base
		dto.File = &fd
	}
	return dto
}

type tokenDTO struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	CreatedAt string   `json:"created_at"`
	ExpiresAt *string  `json:"expires_at"`
	LastUsed  *string  `json:"last_used"`
	// Secret is returned exactly once, by the call that creates the token. It
	// is not recoverable afterwards.
	Secret string `json:"secret,omitempty"`
}

func (s *Server) tokenDTO(t *store.Token, secret string) tokenDTO {
	return tokenDTO{
		ID:        t.ID,
		Name:      t.Name,
		Scopes:    t.Scopes,
		CreatedAt: timeStr(t.CreatedAt),
		ExpiresAt: timePtrStr(t.ExpiresAt),
		LastUsed:  timePtrStr(t.LastUsed),
		Secret:    secret,
	}
}
