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
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Size        int64           `json:"size"`
	Sha256      string          `json:"sha256"`
	ContentType string          `json:"content_type"`
	Tags        []string        `json:"tags,omitempty"`
	Meta        json.RawMessage `json:"meta,omitempty"`
	Owner       string          `json:"owner,omitempty"`
	CreatedAt   string          `json:"created_at"`
	ExpiresAt   *string         `json:"expires_at"`
	Downloads   int64           `json:"downloads"`
	URLs        fileURLs        `json:"urls"`
}

func (s *Server) fileDTO(c *gin.Context, f *store.File) fileDTO {
	dto := fileDTO{
		ID:          f.ID,
		Name:        f.Name,
		Size:        f.Size,
		Sha256:      f.Sha256,
		ContentType: f.ContentType,
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

type shareDTO struct {
	ID           string   `json:"id"`
	URL          string   `json:"url"`
	FileID       string   `json:"file_id"`
	Label        string   `json:"label,omitempty"`
	Owner        string   `json:"owner,omitempty"`
	CreatedAt    string   `json:"created_at"`
	ExpiresAt    *string  `json:"expires_at"`
	MaxDownloads *int64   `json:"max_downloads"`
	Downloads    int64    `json:"downloads"`
	File         *fileDTO `json:"file,omitempty"`
}

func (s *Server) shareDTO(c *gin.Context, sh *store.Share, f *store.File) shareDTO {
	dto := shareDTO{
		ID:           sh.ID,
		URL:          s.absURL(c, "/s/"+sh.ID),
		FileID:       sh.FileID,
		Label:        sh.Label,
		Owner:        sh.Owner,
		CreatedAt:    timeStr(sh.CreatedAt),
		ExpiresAt:    timePtrStr(sh.ExpiresAt),
		MaxDownloads: sh.MaxDownloads,
		Downloads:    sh.Downloads,
	}
	if f != nil {
		fd := s.fileDTO(c, f)
		fd.URLs.Public = dto.URL
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
