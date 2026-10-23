package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

const (
	pathMarketingTouch       = "/api/v1/marketing/touch"
	pathMarketingTouchAction = "/api/v1/marketing/touch/action"
)

func marketingHeaders(cred *Credential) map[string]string {
	h := map[string]string{
		"Accept": "application/json", "User-Agent": UserAgent,
		"X-Client-Language": "zh-CN", "X-ZCode-App-Version": ClientAppVersion,
	}
	BuildIdentityHeaders(cred.Profile).Apply(h)
	if strings.TrimSpace(cred.JWTToken) != "" {
		h["Authorization"] = "Bearer " + cred.JWTToken
	}
	return h
}

type marketingTouchResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Deliveries []struct {
			Banner struct {
				Background struct {
					Args struct {
						Plan struct {
							PlanID       string  `json:"plan_id"`
							Name         string  `json:"name"`
							Priority     int     `json:"priority"`
							StartsAt     float64 `json:"starts_at"`
							EndsAt       float64 `json:"ends_at"`
							Entitlements []struct {
								ShowName   string  `json:"show_name"`
								GrantUnits float64 `json:"grant_units"`
								Period     string  `json:"period"`
							} `json:"entitlements"`
						} `json:"zcode_plan"`
					} `json:"args"`
				} `json:"background"`
			} `json:"banner"`
		} `json:"deliveries"`
	} `json:"data"`
}

func queryMarketingTouch(ctx context.Context, cred *Credential) ([]core.PlanInfo, error) {
	u, err := url.Parse(ZCodeOrigin + pathMarketingTouch)
	if err != nil {
		return nil, err
	}
	u.RawQuery = "seq=0"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range marketingHeaders(cred) {
		req.Header.Set(k, v)
	}
	resp, err := hostbridge.Client(ctx, billingHTTPTimeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload marketingTouchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, billingMaxBody)).Decode(&payload); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || payload.Code != 0 {
		return nil, fmt.Errorf("营销活动查询失败: %s", payload.Msg)
	}
	out := make([]core.PlanInfo, 0, len(payload.Data.Deliveries))
	for _, delivery := range payload.Data.Deliveries {
		p := delivery.Banner.Background.Args.Plan
		if strings.TrimSpace(p.PlanID) == "" {
			continue
		}
		plan := map[string]any{"plan_id": p.PlanID, "name": p.Name, "priority": float64(p.Priority), "starts_at": p.StartsAt, "ends_at": p.EndsAt}
		grants := make([]any, 0, len(p.Entitlements))
		for _, entitlement := range p.Entitlements {
			grants = append(grants, map[string]any{"show_name": entitlement.ShowName, "grant_units": entitlement.GrantUnits, "period": entitlement.Period})
		}
		plan["entitlements"] = grants
		if parsed := parsePlan(plan); parsed != nil {
			out = append(out, *parsed)
		}
	}
	return out, nil
}
