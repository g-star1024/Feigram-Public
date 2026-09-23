package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleAuth 实现 Go Telegram Core 鉴权 API（M2.1 骨架）。
//
// 路由（均挂载在既有 3090 mux 的 /api/auth/ 子树下，复用 native 登录能力；
// 端口 3091 的独立监听按重构方案 02 §4.1 在 M2.2 拆分）：
//
//	POST /api/auth/start     {userID, accountID, phone, apiId, apiHash} -> 发送验证码
//	POST /api/auth/code      {loginId, code}                            -> 提交验证码
//	POST /api/auth/password  {loginId, password}                        -> 提交两步验证密码
//	POST /api/auth/qr/start  {userID, accountID, apiId, apiHash}        -> 发起二维码登录
//	POST /api/auth/qr/status {loginId}                                  -> 轮询二维码登录状态
//
// 直接复用既有 startNativeLogin / continueNativeLogin / startNativeQRLogin / pollNativeQRLogin，
// 不重复实现 gotd 鉴权流程，避免引入新 bug。
func (a *App) handleAuth(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/api/auth/")
	action = strings.Trim(action, "/")

	switch action {
	case "start":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		var in struct {
			UserID    string `json:"userID"`
			AccountID string `json:"accountID"`
			Phone     string `json:"phone"`
			APIID     int    `json:"apiId"`
			APIHash   string `json:"apiHash"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := a.startNativeLogin(in.UserID, in.AccountID, in.Phone, in.APIID, in.APIHash)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"loginId":          result.LoginID,
			"passwordRequired": result.PasswordRequired,
			"done":             result.Done,
			"account":          publicNativeAccount(result.Account),
		})

	case "code", "password":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		var in struct {
			LoginID  string `json:"loginId"`
			Code     string `json:"code"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := a.continueNativeLogin(in.LoginID, action, in.Code, in.Password)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"loginId":          result.LoginID,
			"passwordRequired": result.PasswordRequired,
			"done":             result.Done,
			"account":          publicNativeAccount(result.Account),
		})

	case "qr/start":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		var in struct {
			UserID    string `json:"userID"`
			AccountID string `json:"accountID"`
			APIID     int    `json:"apiId"`
			APIHash   string `json:"apiHash"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := a.startNativeQRLogin(in.UserID, in.AccountID, in.APIID, in.APIHash)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)

	case "qr/status":
		var in struct {
			LoginID string `json:"loginId"`
		}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		} else {
			in.LoginID = r.URL.Query().Get("loginId")
		}
		result, err := a.pollNativeQRLogin(in.LoginID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown auth action: " + action})
	}
}
