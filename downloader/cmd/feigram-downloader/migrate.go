package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/session"
)

// handleMigrate 实现 GramJS → gotd session 「一键迁移」（M2.4 路径 A）。
//
// 路由：POST /api/accounts/migrate
//
//	请求 {userID, accountID, gramjsSession, apiId, apiHash, phone, label}
//	成功 {migrated:true, dc, account}
//	失败 HTTP 500 + {migrated:false, error, account} —— 前端据此降级「路径 B：重新登录」
//
// 实现要点（均已对照源码确认，非推测）：
//  1. GramJS StringSession.save() 的输出布局为
//     "1" + base64( dcId(1B) | addrLen(2B 大端) | addr | port(2B 大端) | authKey(256B) )
//     依据：telegram/sessions/StringSession.js 的 save()/constructor()。
//  2. auth_key_id 必须用 gotd 自己的 crypto.Key.ID() 计算，保证与
//     gotd telegram/session.go restoreConnection 中
//     `key.Value.ID() != key.ID → "corrupted key"` 的校验完全一致。
//  3. session payload 交由 session.Loader.Save() 序列化（{"Version":1,"Data":{...}}），
//     不手工拼接 JSON，避免与 gotd 版本格式漂移。
//  4. 迁移后立刻执行 nativeHealthCheck：通过才算路径 A 成功，否则降级路径 B。
//     注意 gotd 的 restoreConnection 仅把 Addr 用于日志，DC 解析走 dcList() 回退，
//     与全新登录同一路径，因此空 Config 不影响连通性判定。
func (a *App) handleMigrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}

	var in struct {
		UserID        string `json:"userID"`
		AccountID     string `json:"accountID"`
		GramJSSession string `json:"gramjsSession"`
		Phone         string `json:"phone"`
		Label         string `json:"label"`
		APIID         int    `json:"apiId"`
		APIHash       string `json:"apiHash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求解析失败: " + err.Error()})
		return
	}
	if in.UserID == "" || in.AccountID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 userID/accountID"})
		return
	}
	if in.APIID <= 0 || in.APIHash == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "迁移必须使用自备的 apiId/apiHash（合规要求，不接受内置凭据）"})
		return
	}

	dc, addr, port, authKey, err := parseGramJSStringSession(in.GramJSSession)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GramJS session 解析失败: " + err.Error()})
		return
	}

	// auth_key_id 由 gotd 计算，确保与 restoreConnection 校验同源。
	var key crypto.AuthKey
	copy(key.Value[:], authKey)
	keyID := key.Value.ID()

	loader := session.Loader{Storage: nativeSessionStorage{app: a, userID: in.UserID, accountID: in.AccountID}}
	if err := loader.Save(r.Context(), &session.Data{
		DC:        dc,
		Addr:      fmt.Sprintf("%s:%d", addr, port),
		AuthKey:   authKey,
		AuthKeyID: keyID[:],
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gotd session 写入失败: " + err.Error()})
		return
	}

	// 补齐凭据与展示信息，使后续健康检查可用（StoreSession 只写 Session 字段）。
	a.mu.Lock()
	snapshot, upsertErr := a.upsertNativeAccountLocked(NativeAccount{
		UserID:      in.UserID,
		AccountID:   in.AccountID,
		Phone:       in.Phone,
		DisplayName: in.Label,
		APIID:       in.APIID,
		APIHash:     in.APIHash,
	})
	if upsertErr == nil {
		upsertErr = a.saveNativeLocked()
	}
	a.mu.Unlock()
	if upsertErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "账号元数据写入失败: " + upsertErr.Error()})
		return
	}

	// 迁移后立刻健康检查：未通过即判定路径 A 失败，交由前端降级路径 B。
	checked, err := a.nativeHealthCheck(snapshot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"migrated": false,
			"error":    "迁移后健康检查未通过，请改用重新登录: " + err.Error(),
			"account":  publicNativeAccount(snapshot),
		})
		return
	}
	if !checked.Ready {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"migrated": false,
			"error":    "迁移后 session 未授权，请改用重新登录",
			"account":  publicNativeAccount(checked),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"migrated": true,
		"dc":       dc,
		"account":  publicNativeAccount(checked),
	})
}

// parseGramJSStringSession 解析 GramJS StringSession.save() 产出的字符串。
//
// 布局（版本前缀 "1" 之后为 base64）：
//
//	dcId(1B) | addrLen(2B 大端) | addr(addrLen B) | port(2B 大端) | authKey(剩余, 应 256B)
//
// 仅支持 save() 写出的标准格式；Telethon/IPv6 原始字节变体在 save() 中不会产生，
// 因此遇到异常长度直接报错，交由前端降级重新登录。
func parseGramJSStringSession(raw string) (dc int, addr string, port int, authKey []byte, err error) {
	if len(raw) < 2 {
		return 0, "", 0, nil, fmt.Errorf("session 为空")
	}
	if raw[0] != '1' {
		return 0, "", 0, nil, fmt.Errorf("不支持的 session 版本前缀 %q（预期 \"1\"）", raw[:1])
	}
	buf, err := base64.StdEncoding.DecodeString(raw[1:])
	if err != nil {
		return 0, "", 0, nil, fmt.Errorf("base64 解码失败: %w", err)
	}
	if len(buf) < 5 {
		return 0, "", 0, nil, fmt.Errorf("解码后长度 %d 不足", len(buf))
	}
	dc = int(buf[0])
	addrLen := int(binary.BigEndian.Uint16(buf[1:3]))
	if addrLen <= 0 || addrLen > 100 {
		return 0, "", 0, nil, fmt.Errorf("地址长度 %d 异常（仅支持 save() 写出的 IPv4/域名格式）", addrLen)
	}
	if len(buf) < 3+addrLen+2 {
		return 0, "", 0, nil, fmt.Errorf("长度 %d 与地址长度 %d 不匹配", len(buf), addrLen)
	}
	addr = string(buf[3 : 3+addrLen])
	port = int(binary.BigEndian.Uint16(buf[3+addrLen : 5+addrLen]))
	authKey = buf[5+addrLen:]
	if len(authKey) != 256 {
		return 0, "", 0, nil, fmt.Errorf("auth_key 长度 %d 异常（预期 256）", len(authKey))
	}
	if dc <= 0 {
		return 0, "", 0, nil, fmt.Errorf("dcId %d 非法", dc)
	}
	return dc, addr, port, authKey, nil
}
