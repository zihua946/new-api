package mcp

import (
	"encoding/hex"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	"strings"
)

type TrustedApplicationView struct {
	ID                 string   `json:"id"`
	Revision           int64    `json:"revision"`
	Enabled            bool     `json:"enabled"`
	ServerIDs          []string `json:"server_ids"`
	MaxChargeQuota     int      `json:"max_charge_quota"`
	ExchangeConfigured bool     `json:"exchange_configured"`
	CasdoorConfigured  bool     `json:"casdoor_configured"`
}
type TrustedApplicationInput struct {
	ExpectedRevision    int64    `json:"expected_revision"`
	Enabled             bool     `json:"enabled"`
	ServerIDs           []string `json:"server_ids"`
	MaxChargeQuota      int      `json:"max_charge_quota"`
	ExchangeSecret      *string  `json:"exchange_secret"`
	CasdoorClientSecret *string  `json:"casdoor_client_secret"`
}

func ListTrustedApplications() ([]TrustedApplicationView, error) {
	var rows []model.MCPTrustedApplication
	if err := model.DB.Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]TrustedApplicationView, 0, len(rows))
	for _, row := range rows {
		raw, err := Open(string(row.Payload), "mcp-app:"+row.ID)
		if err != nil {
			return nil, err
		}
		var app trustedMCPApplication
		if err = common.UnmarshalJsonStr(raw, &app); err != nil {
			return nil, err
		}
		views = append(views, TrustedApplicationView{row.ID, row.Revision, app.Enabled, app.ServerIDs, app.MaxChargeQuota, app.ExchangeSecretSHA256 != "", app.CasdoorClientSecret != ""})
	}
	return views, nil
}
func SaveTrustedApplication(id string, input TrustedApplicationInput) error {
	if strings.TrimSpace(id) != id || id == "" || len(id) > 256 || strings.ContainsAny(id, "/\\\x00") || input.ExpectedRevision < 0 || input.MaxChargeQuota <= 0 || input.MaxChargeQuota > common.MaxQuota || len(input.ServerIDs) == 0 || len(input.ServerIDs) > 100 {
		return Failure(422, "INVALID_APPLICATION", "Invalid trusted application configuration")
	}
	var app trustedMCPApplication
	if input.ExpectedRevision > 0 {
		var row model.MCPTrustedApplication
		if err := model.DB.First(&row, "id = ? AND revision = ?", id, input.ExpectedRevision).Error; err != nil {
			return model.ErrMCPRevisionConflict
		}
		raw, err := Open(string(row.Payload), "mcp-app:"+id)
		if err != nil {
			return err
		}
		if err = common.UnmarshalJsonStr(raw, &app); err != nil {
			return err
		}
	}
	app.Enabled, app.ServerIDs, app.MaxChargeQuota = input.Enabled, input.ServerIDs, input.MaxChargeQuota
	for _, serverID := range input.ServerIDs {
		if _, err := model.GetMCPServer(serverID); err != nil {
			return Failure(422, "INVALID_APPLICATION", "Select existing MCP services")
		}
	}
	if input.ExchangeSecret != nil {
		if *input.ExchangeSecret == "" {
			app.ExchangeSecretSHA256 = ""
		} else {
			if len(*input.ExchangeSecret) < 32 || len(*input.ExchangeSecret) > 4096 {
				return Failure(422, "INVALID_APPLICATION", "Exchange secret must contain at least 32 characters")
			}
			app.ExchangeSecretSHA256 = Digest(*input.ExchangeSecret)
		}
	}
	app.Generation = uuid.NewString()

	if input.CasdoorClientSecret != nil {
		if len(*input.CasdoorClientSecret) > 4096 {
			return Failure(422, "INVALID_APPLICATION", "Application secret is too long")
		}
		app.CasdoorClientSecret = *input.CasdoorClientSecret
	}
	hash, e := hex.DecodeString(app.ExchangeSecretSHA256)
	if app.Enabled && (e != nil || len(hash) != 32 || app.CasdoorClientSecret == "") {
		return Failure(422, "INVALID_APPLICATION", "Configure both application credentials before enabling")
	}
	raw, err := common.Marshal(app)
	if err != nil {
		return err
	}
	cipher, err := Seal(string(raw), "mcp-app:"+id)
	if err != nil {
		return err
	}
	if input.ExpectedRevision == 0 {
		return model.DB.Create(&model.MCPTrustedApplication{ID: id, Revision: 1, Payload: model.MCPEncryptedResult(cipher)}).Error
	}
	result := model.DB.Model(&model.MCPTrustedApplication{}).Where("id = ? AND revision = ?", id, input.ExpectedRevision).Updates(map[string]any{"revision": input.ExpectedRevision + 1, "payload": cipher})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return model.ErrMCPRevisionConflict
	}
	return nil
}
func DeleteTrustedApplication(id string, revision int64) error {
	if revision <= 0 {
		return model.ErrMCPRevisionConflict
	}
	result := model.DB.Where("id = ? AND revision = ?", id, revision).Delete(&model.MCPTrustedApplication{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return model.ErrMCPRevisionConflict
	}
	return nil
}
