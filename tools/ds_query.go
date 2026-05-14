package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	mcpgrafana "github.com/grafana/mcp-grafana"
)

const dsQueryResponseLimit int64 = 10 * 1024 * 1024 // 10MB

// doDSQuery posts a payload to Grafana's /api/ds/query endpoint and decodes
// the response into the SDK's QueryDataResponse type.
func doDSQuery(ctx context.Context, client *http.Client, baseURL string, payload map[string]interface{}) (*backend.QueryDataResponse, error) {
	return doDSQueryWithLimit(ctx, client, baseURL, payload, dsQueryResponseLimit)
}

// doDSQueryWithLimit is like doDSQuery but allows overriding the response size limit.
func doDSQueryWithLimit(ctx context.Context, client *http.Client, baseURL string, payload map[string]interface{}, responseLimit int64) (*backend.QueryDataResponse, error) {
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query returned status %d: %s", resp.StatusCode, string(body[:min(len(body), 1024)]))
	}

	var queryResp backend.QueryDataResponse
	if err := json.Unmarshal(body, &queryResp); err != nil {
		return nil, fmt.Errorf("unmarshaling response: %w", err)
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

	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, baseURL, nil
}

// normalizeValue converts SDK-typed values back to the JSON-basic types that
// standard json.Unmarshal into interface{} would produce. This preserves the
// MCP tool output contract established by the original per-datasource code
// which decoded into [][]interface{} (yielding float64 for all numbers,
// string for strings, nil for nulls, and no time.Time or pointer wrappers).
func normalizeValue(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case time.Time:
		return float64(val.UnixMilli())
	case *time.Time:
		if val == nil {
			return nil
		}
		return float64(val.UnixMilli())
	case json.RawMessage:
		var decoded interface{}
		if json.Unmarshal(val, &decoded) == nil {
			return decoded
		}
		return string(val)
	case float64, string, bool:
		return v
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Ptr {
			if rv.IsNil() {
				return nil
			}
			return normalizeValue(rv.Elem().Interface())
		}
		switch rv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return float64(rv.Int())
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return float64(rv.Uint())
		case reflect.Float32:
			return rv.Float()
		}
		return v
	}
}

// framesToTabularRows converts SDK data frames into row-oriented maps — the
// common format returned by ClickHouse, Snowflake, and Athena tools.
func framesToTabularRows(resp *backend.QueryDataResponse) ([]string, []map[string]interface{}, error) {
	columns := []string{}
	rows := []map[string]interface{}{}

	for refID, r := range resp.Responses {
		if r.Error != nil {
			return nil, nil, fmt.Errorf("query error (refId=%s): %s", refID, r.Error)
		}

		for _, frame := range r.Frames {
			cols := make([]string, len(frame.Fields))
			for i, field := range frame.Fields {
				cols[i] = field.Name
			}
			columns = cols

			rowCount := frame.Rows()
			for i := 0; i < rowCount; i++ {
				row := make(map[string]interface{})
				for colIdx, colName := range cols {
					row[colName] = normalizeValue(frame.At(colIdx, i))
				}
				rows = append(rows, row)
			}
		}
	}

	return columns, rows, nil
}
