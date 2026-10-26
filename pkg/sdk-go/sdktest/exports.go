package sdktest

import (
	"net/http"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func (c *call) exportCreate() {
	sessionID := c.param("session_id")
	if sessionID != c.claims.SessionID {
		c.fail(http.StatusForbidden, contracts.ErrScopeViolation, "the session is not the token's")
		return
	}
	if !contains(c.claims.CapabilityScopes, "exports.create") {
		c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "the context does not permit exports.create")
		return
	}
	var body conversation.ExportRequest
	if !c.providersDecode(&body) {
		return
	}
	if body.TurnID == "" || !body.Format.Valid() {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "turn_id and format are required")
		return
	}
	if strings.HasSuffix(string(body.TurnID), "FA0") {
		c.fail(http.StatusNotFound, contracts.ErrValidationFailed, "the turn is not available for export", "reason", "not_found")
		return
	}
	mime := map[conversation.ExportRequestFormat]string{
		conversation.ExportRequestFormatCsv:  "text/csv",
		conversation.ExportRequestFormatXlsx: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		conversation.ExportRequestFormatHtml: "text/html",
		conversation.ExportRequestFormatMd:   "text/markdown",
	}[body.Format]
	ext := string(body.Format)
	if body.Format == conversation.ExportRequestFormatMd {
		ext = "md"
	}
	id := "fil_01J0000000000000000000000E"
	c.w.Header().Set("Location", "/v1/files/"+id)
	c.json(http.StatusCreated, conversation.FileRef{
		FileID:      actions.FileID(id),
		Name:        "export." + ext,
		MimeType:    mime,
		SizeBytes:   32,
		Owner:       common.Identifier(c.claims.PrincipalID),
		Origin:      conversation.FileRefOriginGenerated,
		Permissions: []conversation.FileRefPermissionsItem{conversation.FileRefPermissionsItemRead, conversation.FileRefPermissionsItemDownload},
		Previewable: body.Format != conversation.ExportRequestFormatXlsx,
		ExpiresAt:   common.Timestamp(time.Now().UTC().Add(time.Hour).Format(time.RFC3339)),
	})
}
