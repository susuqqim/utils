package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/selfupdate"
)

func callUpdate(t *testing.T, p *Panel, method, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	switch path {
	case "/panel/api/update/progress":
		p.handleUpdateProgress(rec, req)
	default:
		p.handleUpdateApply(rec, req)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

// 未装配自更新时，progress 回 idle、apply 回 501 —— 面板据此不显示"立即更新"。
func TestUpdateProgressIdleWhenUnwired(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	rec, body := callUpdate(t, p, http.MethodGet, "/panel/api/update/progress")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if body["stage"] != selfupdate.StageIdle {
		t.Fatalf("stage=%v want idle", body["stage"])
	}

	rec, body = callUpdate(t, p, http.MethodPost, "/panel/api/update/apply")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501", rec.Code)
	}
	if body["code"] != "not_applicable" {
		t.Fatalf("code=%v want not_applicable", body["code"])
	}
}

// 进度透传：hook 返回什么就回什么（面板进度条数据源）。
func TestUpdateProgressWired(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", UpdateProgress: func() selfupdate.State {
		return selfupdate.State{Stage: selfupdate.StageDownload, Percent: 42, To: "1.11.20"}
	}})
	rec, body := callUpdate(t, p, http.MethodGet, "/panel/api/update/progress")
	if rec.Code != http.StatusOK || body["stage"] != selfupdate.StageDownload {
		t.Fatalf("code=%d body=%v", rec.Code, body)
	}
	if int(body["percent"].(float64)) != 42 {
		t.Fatalf("percent=%v want 42", body["percent"])
	}
}

// 忙碌（已有更新在跑）→ 409；在途请求 → 409 且原因可读。
func TestUpdateApplyConflictCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"busy", selfupdate.ErrBusy, "update_busy"},
		{"inflight", ErrUpdateInFlight, "update_busy"},
		{"no_msi", selfupdate.ErrNoMSI, "not_applicable"},
		{"async", selfupdate.ErrNotRunnable, "not_applicable"},
		{"scheme", selfupdate.ErrScheme, "bad_manifest"},
		{"other", context.DeadlineExceeded, "update_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyErr := c.err
			p := New(Config{Version: "test", APIKey: "k", ApplyUpdate: func(context.Context) (selfupdate.State, error) {
				return selfupdate.State{Stage: selfupdate.StageFailed}, applyErr
			}})
			rec, body := callUpdate(t, p, http.MethodPost, "/panel/api/update/apply")
			if rec.Code < 400 {
				t.Fatalf("code=%d want >=400", rec.Code)
			}
			if body["code"] != c.code {
				t.Fatalf("code=%v want %v (http=%d)", body["code"], c.code, rec.Code)
			}
			if msg, _ := body["message"].(string); msg == "" {
				t.Error("必须带可读的 message")
			}
		})
	}
}

// 成功：200 + 目标版本（随后网关会退出，由 MSI 拉起新版）。
func TestUpdateApplySuccess(t *testing.T) {
	p := New(Config{Version: "1.11.19", APIKey: "k", ApplyUpdate: func(context.Context) (selfupdate.State, error) {
		return selfupdate.State{Stage: selfupdate.StageInstall, From: "1.11.19", To: "1.11.20", Percent: 100}, nil
	}})
	rec, body := callUpdate(t, p, http.MethodPost, "/panel/api/update/apply")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (%s)", rec.Code, rec.Body.String())
	}
	if body["version"] != "1.11.20" {
		t.Fatalf("version=%v want 1.11.20", body["version"])
	}
	if !strings.Contains(rec.Body.String(), "install") {
		t.Fatalf("响应应带 state.stage=install: %s", rec.Body.String())
	}
}
