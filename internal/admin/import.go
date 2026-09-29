//
//
// Tencent is pleased to support the open source community by making Tencent Cloud AIGate available.
//
// Copyright (C) 2026 Tencent.
// All rights reserved.
//
// If you have downloaded a copy of the Tencent Cloud AIGate source code from Tencent,
// please note that Tencent Cloud AIGate source code is licensed under the Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// 导入条目的种类与动作（响应 details[].kind / details[].action 的取值）。
const (
	importKindGroup = "group"
	importKindUser  = "user"
	importKindModel = "model"
	importKindKey   = "key"

	importActionCreated = "created"
	importActionSkipped = "skipped"
	importActionError   = "error"
)

// importRequest 是 `POST /admin/import` 的请求体。
//
// 处理顺序固定为 groups → users → models → keys：后者的引用可以指向本次刚创建的实体。
// dry_run=true 时只做校验与计划，不写入任何数据。
type importRequest struct {
	DryRun     bool          `json:"dry_run"`
	UserGroups []importGroup `json:"user_groups"`
	Users      []importUser  `json:"users"`
	Models     []importModel `json:"models"`
	Keys       []importKey   `json:"keys"`
}

// importGroup 是待导入的分组；name 为幂等键。
type importGroup struct {
	Name            string `json:"name"`
	TokenQuotaRules string `json:"token_quota_rules"`
}

// importUser 是待导入的用户；group 按名字引用（可指向库中已有分组）。
// 导入不返回用户访问 token：api_token 哈希随机生成且明文丢弃，需要时用 rotate 换发。
type importUser struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Group    string `json:"group"`
}

// importModel 是待导入的模型；(provider, model_id) 为幂等键。
type importModel struct {
	Provider  string `json:"provider"`
	ModelID   string `json:"model_id"`
	ModelName string `json:"model_name"`
	URL       string `json:"url"`
	APIKey    string `json:"api_key"`
	ModelType string `json:"model_type"`
	QuotaDay  *int   `json:"quota_day"`
	Enabled   *bool  `json:"enabled"`
}

// importKey 是待导入的数据面密钥；token 为明文（哈希后比对幂等键），user/group 按名字引用。
type importKey struct {
	Name      string          `json:"name"`
	User      string          `json:"user"`
	Group     string          `json:"group"`
	MaxTokens int             `json:"max_tokens"`
	Token     string          `json:"token"`
	Bindings  []importBinding `json:"bindings"`
}

// importBinding 是密钥到模型的绑定；model 按 model_id 引用。
// APIKey 为可选绑定级上游密钥（自备 Key 模型迁移用；加密落库）。
type importBinding struct {
	Model  string `json:"model"`
	Role   string `json:"role"`
	APIKey string `json:"api_key"`
}

// importSummary 是导入结果的计数汇总。
type importSummary struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"`
	Errors  int `json:"errors"`
}

// importDetail 是单条导入条目的结果明细。
// ref 只承载非敏感标识（名字 / 模型 provider+model_id / token 展示前缀），绝不回显 token 明文。
type importDetail struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Action  string `json:"action"`
	Message string `json:"message,omitempty"`
}

// importResponse 是 `POST /admin/import` 的响应体。
type importResponse struct {
	DryRun  bool           `json:"dry_run"`
	Summary importSummary  `json:"summary"`
	Details []importDetail `json:"details"`
}

// importRun 承载一次导入的进度：既有实体的解析表 + 逐条结果。
// 同一份 payload 内重复出现的幂等键按「已存在」处理（第二次记为 skipped）。
type importRun struct {
	ctx    context.Context
	store  *model.Store
	dryRun bool

	summary importSummary
	details []importDetail

	groupIDs  map[string]uint     // 分组名 → ID
	userIDs   map[string]uint     // 用户名 → ID
	modelIDs  map[string]uint     // provider/model_id → ID（模型幂等键）
	modelRefs map[string]uint     // model_id → ID（keys[].bindings[].model 的解析表）
	keyHashes map[string]struct{} // 密钥 token 哈希
}

// handleImport 实现 `POST /admin/import`：一次性导入存量（用户/分组/模型/密钥）。
//
// 语义要点：
//   - dry_run=true 只返回计划，不写库；
//   - 单条失败（引用缺失 / 非法字段 / 非法配额规则）记入 details 并继续处理其余条目；
//   - 请求本身成功即返回 200，逐条结果在 body 的 summary/details 中；
//   - token 明文只用于计算哈希与展示前缀，不落库、不回显、不写日志。
func handleImport(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req importRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		run, err := newImportRun(r.Context(), store, req.DryRun)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		run.importGroups(req.UserGroups)
		run.importUsers(req.Users)
		run.importModels(req.Models)
		run.importKeys(req.Keys)

		details := run.details
		if details == nil {
			details = []importDetail{}
		}
		httpx.WriteJSON(w, http.StatusOK, importResponse{
			DryRun:  req.DryRun,
			Summary: run.summary,
			Details: details,
		})
	}
}

// newImportRun 预载既有实体的幂等键（分组名 / 用户名 / provider+model_id / token 哈希）。
func newImportRun(ctx context.Context, store *model.Store, dryRun bool) (*importRun, error) {
	run := &importRun{
		ctx:       ctx,
		store:     store,
		dryRun:    dryRun,
		groupIDs:  map[string]uint{},
		userIDs:   map[string]uint{},
		modelIDs:  map[string]uint{},
		modelRefs: map[string]uint{},
		keyHashes: map[string]struct{}{},
	}

	groups, err := store.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		run.groupIDs[groups[i].Name] = groups[i].ID
	}

	users, err := store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		run.userIDs[users[i].Username] = users[i].ID
	}

	models, err := store.ListAIModels(ctx)
	if err != nil {
		return nil, err
	}
	for i := range models {
		run.modelIDs[importModelKey(models[i].Provider, models[i].ModelID)] = models[i].ID
		// 绑定按 model_id 引用：同 model_id 出现在多个 provider 时以先出现者为准（幂等重跑稳定）。
		if _, ok := run.modelRefs[models[i].ModelID]; !ok {
			run.modelRefs[models[i].ModelID] = models[i].ID
		}
	}

	keys, err := store.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if keys[i].ProxyTokenHash != "" {
			run.keyHashes[keys[i].ProxyTokenHash] = struct{}{}
		}
	}
	return run, nil
}

// record 记录一条结果明细并同步计数。
func (r *importRun) record(kind, ref, action, message string) {
	r.details = append(r.details, importDetail{Kind: kind, Ref: ref, Action: action, Message: message})
	switch action {
	case importActionCreated:
		r.summary.Created++
	case importActionSkipped:
		r.summary.Skipped++
	case importActionError:
		r.summary.Errors++
	}
}

func (r *importRun) importGroups(items []importGroup) {
	for _, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			r.record(importKindGroup, "", importActionError, "name is required")
			continue
		}
		if _, exists := r.groupIDs[name]; exists {
			r.record(importKindGroup, name, importActionSkipped, "group already exists")
			continue
		}
		rules, err := normalizeGroupRules(item.TokenQuotaRules)
		if err != nil {
			r.record(importKindGroup, name, importActionError, err.Error())
			continue
		}
		if r.dryRun {
			r.groupIDs[name] = 0 // 预演占位：仅用于后续引用可解析
			r.record(importKindGroup, name, importActionCreated, "")
			continue
		}
		g := &model.Group{Name: name, TokenQuotaRules: rules}
		if err := r.store.CreateGroup(r.ctx, g); err != nil {
			r.record(importKindGroup, name, importActionError, err.Error())
			continue
		}
		r.groupIDs[name] = g.ID
		r.record(importKindGroup, name, importActionCreated, "")
	}
}

func (r *importRun) importUsers(items []importUser) {
	for _, item := range items {
		name := strings.TrimSpace(item.Username)
		if name == "" {
			r.record(importKindUser, "", importActionError, "name is required")
			continue
		}
		if _, exists := r.userIDs[name]; exists {
			r.record(importKindUser, name, importActionSkipped, "user already exists")
			continue
		}
		role := strings.TrimSpace(item.Role)
		if role == "" {
			role = model.RoleUser
		}
		if !validRole(role) {
			r.record(importKindUser, name, importActionError, "role must be admin or user")
			continue
		}
		groupID, ok := r.resolveGroup(item.Group)
		if !ok {
			r.record(importKindUser, name, importActionError, "group not found: "+strings.TrimSpace(item.Group))
			continue
		}
		if r.dryRun {
			r.userIDs[name] = 0
			r.record(importKindUser, name, importActionCreated, "")
			continue
		}
		// 明文 token 生成后立即丢弃：既保证 token 哈希唯一索引不冲突，也不对外泄露。
		_, hash, prefix, err := model.GenerateToken(model.TokenPrefixAccess)
		if err != nil {
			r.record(importKindUser, name, importActionError, err.Error())
			continue
		}
		u := &model.User{
			Username:       name,
			Role:           role,
			GroupID:        groupID,
			APITokenHash:   hash,
			APITokenPrefix: prefix,
			Enabled:        true,
		}
		if err := r.store.CreateUser(r.ctx, u); err != nil {
			r.record(importKindUser, name, importActionError, err.Error())
			continue
		}
		r.userIDs[name] = u.ID
		r.record(importKindUser, name, importActionCreated, "")
	}
}

func (r *importRun) importModels(items []importModel) {
	for _, item := range items {
		modelID := strings.TrimSpace(item.ModelID)
		provider := strings.TrimSpace(item.Provider)
		ref := importModelRef(provider, modelID)
		if modelID == "" {
			r.record(importKindModel, ref, importActionError, "model_id is required")
			continue
		}
		if _, exists := r.modelIDs[importModelKey(provider, modelID)]; exists {
			r.record(importKindModel, ref, importActionSkipped, "model already exists")
			continue
		}
		if strings.TrimSpace(item.URL) == "" {
			r.record(importKindModel, ref, importActionError, "url is required")
			continue
		}
		if !validModelType(item.ModelType) {
			r.record(
				importKindModel,
				ref,
				importActionError,
				"model_type must be openai-completions or anthropic-messages",
			)
			continue
		}
		m := &model.AIModel{
			Provider:  provider,
			ModelID:   modelID,
			ModelName: item.ModelName,
			URL:       item.URL,
			APIKey:    item.APIKey,
			ModelType: item.ModelType,
			QuotaDay:  -1,
			Enabled:   true,
		}
		if item.QuotaDay != nil {
			m.QuotaDay = *item.QuotaDay
		}
		if item.Enabled != nil {
			m.Enabled = *item.Enabled
		}
		if r.dryRun {
			r.modelIDs[importModelKey(provider, modelID)] = 0
			r.registerModelRef(modelID, 0)
			r.record(importKindModel, ref, importActionCreated, "")
			continue
		}
		if err := r.store.CreateAIModel(r.ctx, m); err != nil {
			r.record(importKindModel, ref, importActionError, err.Error())
			continue
		}
		// 带 default 标签的零值字段在 INSERT 时会被 GORM 跳过，显式零值需补一次定向更新。
		if patch := importModelZeroPatch(item); len(patch) > 0 {
			updated, err := r.store.UpdateAIModel(r.ctx, m.ID, patch)
			if err != nil {
				r.record(importKindModel, ref, importActionError, err.Error())
				continue
			}
			m = updated
		}
		r.modelIDs[importModelKey(provider, modelID)] = m.ID
		r.registerModelRef(modelID, m.ID)
		r.record(importKindModel, ref, importActionCreated, "")
	}
}

// registerModelRef 登记 model_id → ID 的绑定解析项；同 model_id 已存在时不覆盖。
func (r *importRun) registerModelRef(modelID string, id uint) {
	if _, ok := r.modelRefs[modelID]; !ok {
		r.modelRefs[modelID] = id
	}
}

func (r *importRun) importKeys(items []importKey) {
	for _, item := range items {
		token := strings.TrimSpace(item.Token)
		if token == "" {
			r.record(importKindKey, "", importActionError, "token is required")
			continue
		}
		// ref 只承载展示前缀（见 importTokenRef），绝不回显 token 明文。
		ref := importTokenRef(token)
		hash := model.HashToken(token)
		if _, exists := r.keyHashes[hash]; exists {
			r.record(importKindKey, ref, importActionSkipped, "key already exists")
			continue
		}
		userName := strings.TrimSpace(item.User)
		if userName == "" {
			r.record(importKindKey, ref, importActionError, "user is required")
			continue
		}
		userID, ok := r.userIDs[userName]
		if !ok {
			r.record(importKindKey, ref, importActionError, "user not found: "+userName)
			continue
		}
		groupID, ok := r.resolveGroup(item.Group)
		if !ok {
			r.record(importKindKey, ref, importActionError, "group not found: "+strings.TrimSpace(item.Group))
			continue
		}
		if err := validateImportBindings(item.Bindings); err != nil {
			r.record(importKindKey, ref, importActionError, err.Error())
			continue
		}
		bindings, err := r.resolveBindings(item.Bindings)
		if err != nil {
			r.record(importKindKey, ref, importActionError, err.Error())
			continue
		}
		if r.dryRun {
			r.keyHashes[hash] = struct{}{}
			r.record(importKindKey, ref, importActionCreated, "")
			continue
		}
		key := &model.APIKey{
			Name:             item.Name,
			UserID:           userID,
			GroupID:          groupID,
			ProxyTokenHash:   hash,
			ProxyTokenPrefix: ref,
			MaxTokens:        item.MaxTokens,
			Enabled:          true,
		}
		if err := r.store.CreateAPIKey(r.ctx, key, bindings); err != nil {
			r.record(importKindKey, ref, importActionError, err.Error())
			continue
		}
		r.keyHashes[hash] = struct{}{}
		r.record(importKindKey, ref, importActionCreated, "")
	}
}

// resolveGroup 按名字解析分组：空名字视为「不指定分组」（0, true）；
// 名字非空时必须在库中（或本次导入已计划）存在，否则返回 (0, false) 由调用方记为错误。
func (r *importRun) resolveGroup(name string) (uint, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, true
	}
	id, ok := r.groupIDs[name]
	return id, ok
}

// resolveBindings 把 model_id 引用解析为存储层绑定（SortOrder 按出现顺序）。
// 任一引用缺失即返回错误，由调用方记为该条目的错误。
func (r *importRun) resolveBindings(items []importBinding) ([]model.APIKeyModel, error) {
	out := make([]model.APIKeyModel, 0, len(items))
	for i, b := range items {
		ref := strings.TrimSpace(b.Model)
		id, ok := r.modelRefs[ref]
		if !ok {
			return nil, fmt.Errorf("model not found: %s", ref)
		}
		out = append(out, model.APIKeyModel{AIModelID: id, Role: b.Role, SortOrder: i, APIKey: b.APIKey})
	}
	return out, nil
}

// validateImportBindings 校验绑定集合结构（角色合法、恰好一个 primary、无重复模型）。
//
// 复用 model.CreateAPIKey 的既有口径；预演（dry_run）不写库，故此处独立校验，
// 保证预演与真实导入对同一份 payload 给出相同结论。
func validateImportBindings(items []importBinding) error {
	if len(items) == 0 {
		return nil
	}
	primaries := 0
	seen := make(map[string]struct{}, len(items))
	for _, b := range items {
		switch b.Role {
		case model.ModelRolePrimary:
			primaries++
		case model.ModelRoleFallback:
		default:
			return fmt.Errorf("invalid binding role %q", b.Role)
		}
		ref := strings.TrimSpace(b.Model)
		if ref == "" {
			return fmt.Errorf("binding model must not be empty")
		}
		if _, dup := seen[ref]; dup {
			return fmt.Errorf("duplicate model binding: %s", ref)
		}
		seen[ref] = struct{}{}
	}
	if primaries != 1 {
		return fmt.Errorf("bindings must contain exactly one primary model")
	}
	return nil
}

// importTokenRef 生成密钥条目的展示标识（details.ref）：
// 统一复用 DisplayTokenPrefix 的掩码口径（长串首 8 + … + 末 4；短串只回末 4），
// 绝不让 token 明文（或其大部分）出现在响应中。
func importTokenRef(token string) string {
	return model.DisplayTokenPrefix(token)
}

// importModelKey 是模型的幂等键（provider + model_id）。
func importModelKey(provider, modelID string) string {
	return provider + "\x00" + modelID
}

// importModelRef 是人类可读的模型标识（用于 details.ref）。
func importModelRef(provider, modelID string) string {
	switch {
	case provider == "":
		return modelID
	case modelID == "":
		return provider
	default:
		return provider + "/" + modelID
	}
}

// importModelZeroPatch 返回创建后需要补写的零值字段（见 importModels 中说明）。
func importModelZeroPatch(item importModel) map[string]any {
	patch := make(map[string]any)
	if item.Enabled != nil && !*item.Enabled {
		patch["enabled"] = false
	}
	if item.QuotaDay != nil && *item.QuotaDay == 0 {
		patch["quota_day"] = 0
	}
	return patch
}
