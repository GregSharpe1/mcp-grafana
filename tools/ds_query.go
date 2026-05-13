package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	mcpgrafana "github.com/grafana/mcp-grafana"
)

const dsQueryResponseLimit = 10 * 1024 * 1024 // 10MB

// dsQueryResponse represents the standard response from Grafana's /api/ds/query endpoint.
type dsQueryResponse struct {
	Results map[string]dsQueryResult `json:"results"`
}

type dsQueryResult struct {
	Status int            `json:"status,omitempty"`
	Frames []dsQueryFrame `json:"frames,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type dsQueryFrame struct {
	Schema dsQueryFrameSchema `json:"schema"`
	Data   dsQueryFrameData   `json:"data"`
}

type dsQueryFrameSchema struct {
	Name   string              `json:"name,omitempty"`
	RefID  string              `json:"refId,omitempty"`
	Fields []dsQueryFrameField `json:"fields"`
}

type dsQueryFrameField struct {
	Name     string                 `json:"name"`
	Type     string                 `json:"type"`
	Labels   map[string]string      `json:"labels,omitempty"`
	Config   map[string]interface{} `json:"config,omitempty"`
	TypeInfo struct {
		Frame string `json:"frame,omitempty"`
	} `json:"typeInfo,omitempty"`
}

type dsQueryFrameData struct {
	Values [][]interface{} `json:"values"`
}

// doDSQuery posts a payload to Grafana's /api/ds/query endpoint and decodes
// the response into the shared dsQueryResponse type.
func doDSQuery(ctx context.Context, client *http.Client, baseURL string, payload map[string]interface{}) (*dsQueryResponse, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling query payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/ds/query", bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, dsQueryResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query returned status %d: %s", resp.StatusCode, string(body[:min(len(body), 1024)]))
	}

	var queryResp dsQueryResponse
	if err := unmarshalJSONWithLimitMsg(body, &queryResp, dsQueryResponseLimit); err != nil {
		return nil, err
	}

	return &queryResp, nil
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// newDSQueryHTTPClient builds an *http.Client suitable for calling Grafana's
// /api/ds/query endpoint, using the Grafana config from the context.
func newDSQueryHTTPClient(ctx context.Context) (*http.Client, string, error) {
	cfg := mcpgrafana.GrafanaConfigFromContext(ctx)
	baseURL := trimTrailingSlash(cfg.URL)

	transport, err := mcpgrafana.BuildTransport(&cfg, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create transport: %w", err)
	}

	return &http.Client{Transport: transport}, baseURL, nil
}

// framesToTabularRows converts columnar dsQueryFrame data into row-oriented
// maps — the common format returned by ClickHouse, Snowflake, and Athena tools.
// It returns the column names and the flattened rows.
func framesToTabularRows(resp *dsQueryResponse) ([]string, []map[string]interface{}, error) {
	var columns []string
	var rows []map[string]interface{}

	for refID, r := range resp.Results {
		if r.Error != "" {
			return nil, nil, fmt.Errorf("query error (refId=%s): %s", refID, r.Error)
		}

		for _, frame := range r.Frames {
			cols := make([]string, len(frame.Schema.Fields))
			for i, field := range frame.Schema.Fields {
				cols[i] = field.Name
			}
			columns = cols

			if len(frame.Data.Values) == 0 {
				continue
			}

			rowCount := len(frame.Data.Values[0])
			for i := 0; i < rowCount; i++ {
				row := make(map[string]interface{})
				for colIdx, colName := range cols {
					if colIdx < len(frame.Data.Values) && i < len(frame.Data.Values[colIdx]) {
						row[colName] = frame.Data.Values[colIdx][i]
					}
				}
				rows = append(rows, row)
			}
		}
	}

	return columns, rows, nil
}
