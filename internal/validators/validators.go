package validators

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"embyproxy/internal/storage"
)

var NameRE = regexp.MustCompile(`(?i)^[a-z0-9_-]{1,32}$`)
var SecretRE = regexp.MustCompile(`^[^/?#\s]{0,128}$`)
var profileSet = map[string]bool{"yamby": true, "hills_android": true, "hills_windows": true}

type Result struct {
	Node  storage.Node
	Error string
}

func NormalizeName(value any) string {
	return strings.ToLower(strings.TrimSpace(fmt.Sprint(value)))
}

func ValidateName(value any) (string, string) {
	name := NormalizeName(value)
	if !NameRE.MatchString(name) {
		return "", "name 非法：仅允许 a-z / 0-9 / _ / -，长度 1~32"
	}
	return name, ""
}

func ValidateNodeInput(input map[string]any) Result {
	if input == nil {
		return Result{Error: "节点项不是对象"}
	}
	name, errText := ValidateName(input["name"])
	if errText != "" {
		return Result{Error: errText}
	}
	target, errText := ValidateTarget(asString(input["target"]))
	if errText != "" {
		return Result{Error: errText}
	}
	secret, errText := ValidateSecret(input["secret"])
	if errText != "" {
		return Result{Error: errText}
	}
	profile, errText := ValidateImpersonateProfile(input["impersonateProfile"])
	if errText != "" {
		return Result{Error: errText}
	}
	hasImpersonate := hasKey(input, "impersonate")
	return Result{Node: storage.Node{
		Name:               name,
		Target:             target,
		Fav:                ToBool(input["fav"]),
		Secret:             secret,
		Tag:                truncate(strings.TrimSpace(asString(input["tag"])), 64),
		DisplayName:        truncate(regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(asString(input["displayName"])), " "), 32),
		DirectExternal:     ToBool(input["directExternal"]),
		Impersonate:        !hasImpersonate || ToBool(input["impersonate"]),
		ImpersonateProfile: profile,
	}}
}

func ValidateTarget(value string) (string, string) {
	parts := storage.SplitTargets(value)
	if len(parts) == 0 {
		return "", "target 不能为空"
	}
	if len(parts) > 20 {
		return "", "target 数量过多（最多20）"
	}
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, target := range parts {
		if len(target) > 2048 {
			return "", "target 过长"
		}
		u, err := url.Parse(target)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", "target 不是合法 URL: " + target
		}
		if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
			return "", "target 只允许 http/https"
		}
		clean := strings.TrimRight(target, "/")
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	return strings.Join(out, "\n"), ""
}

func ValidateSecret(value any) (string, string) {
	secret := strings.TrimSpace(asString(value))
	if !SecretRE.MatchString(secret) {
		return "", "secret 非法：不能包含 / ? # 或空白字符，最长128"
	}
	return secret, ""
}

func ValidateTag(value any) string {
	return truncate(strings.TrimSpace(asString(value)), 64)
}

func ValidateImpersonateProfile(value any) (string, string) {
	profile := strings.ToLower(strings.TrimSpace(asString(value)))
	if profile == "" {
		profile = "yamby"
	}
	if !profileSet[profile] {
		return "", "伪装身份仅支持 yamby/hills_android/hills_windows"
	}
	return profile, ""
}

func ToBool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "1" || s == "true" || s == "yes" || s == "on"
	default:
		return false
	}
}

func truncate(value string, max int) string {
	if len(value) > max {
		return value[:max]
	}
	return value
}

func asString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}
