// Package i18n localizes the messages the admin API returns to the WebUI.
//
// Only the admin API is localized. The OpenAI-compatible /v1 API keeps its
// English messages on purpose, because clients, SDKs, and proxies may match on
// their text.
//
// English is the source of truth: messages are written in English in the code
// and the catalog only holds the other languages. A missing entry, an
// unsupported Accept-Language, or a language added later therefore never
// changes the English output.
package i18n

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Lang identifies a supported language. The tags match what the WebUI sends in
// Accept-Language and keeps in localStorage.
type Lang string

const (
	// EN is the default and fallback language. It needs no catalog entries,
	// because every message is already written in English.
	EN Lang = "en"
	// ZHCN is Simplified Chinese.
	ZHCN Lang = "zh-CN"
)

// translations holds "English source -> localized text" per language. A source
// containing a fmt verb is a template: the lookup happens first and the
// argument is formatted into the localized template, never into the English one.
var translations = map[Lang]map[string]string{
	ZHCN: {
		// adminAuth
		"missing Authorization header":                  "缺少 Authorization 请求头",
		"invalid admin password":                        "管理密码不正确",
		"too many failed attempts, retry in %d seconds": "失败次数过多，请 %d 秒后重试",

		// request bodies
		"request body is required": "请求体不能为空",

		// accounts
		"account not found":                       "账号不存在",
		"api_key is required":                     "api_key 不能为空",
		"api_key cannot be empty":                 "api_key 不能为空",
		"an account with this key already exists": "该 Key 对应的账号已存在",

		// client keys
		"key not found":                        "密钥不存在",
		"a key with this value already exists": "该密钥值已存在",

		// settings
		"admin_password must be at least 8 characters": "admin_password 至少 8 位",
		"base_url cannot be empty":                     "base_url 不能为空",
		"current admin password is incorrect":          "当前管理密码不正确",

		// OAuth
		"oauth callback url must use http or https":       "回调地址必须使用 http 或 https",
		"oauth callback url must include a host":          "回调地址必须包含主机名",
		"oauth callback url path must end with /callback": "回调地址路径必须以 /callback 结尾",
		"OAuth flow is no longer pending":                 "OAuth 授权流程已不再等待回调",
		"OAuth flow canceled":                             "OAuth 授权流程已取消",
		"state token mismatch (possible tampering)":       "state token 不匹配，可能被篡改",
		"api_key and state are required":                  "缺少必要字段",
		"no pending OAuth flow":                           "当前没有进行中的 OAuth 授权",
		"method not allowed":                              "请求方法不允许",
		"invalid JSON":                                    "JSON 无效",
	},
}

// wrapped holds English prefixes whose remainder (a wrapped error, a path, a
// duration) is produced outside this package and stays as it is. The longest
// matching prefix wins, so a specific entry such as "account added but saving
// config failed: " takes precedence over the generic "saving config failed: ".
var wrapped = map[Lang]map[string]string{
	ZHCN: {
		"saving config failed: ":                   "保存配置失败：",
		"account added but saving config failed: ": "账号已添加，但保存配置失败：",
		"key created but saving config failed: ":   "密钥已创建，但保存配置失败：",
		"applied but saving config failed: ":       "已应用，但保存配置失败：",
		"read request body: ":                      "读取请求体失败：",
		"invalid JSON body: ":                      "JSON 请求体无效：",
		"parse oauth callback url: ":               "解析回调地址失败：",
		"generate oauth state: ":                   "生成 OAuth state 失败：",
		"start callback server: ":                  "启动回调服务器失败：",
		"authorization canceled: ":                 "授权被取消：",
		"OAuth timed out after ":                   "OAuth 授权超时：",
		"decode response: ":                        "解析响应失败：",
	},
}

// Message returns source in lang, formatted with args. Sources that the catalog
// does not know, and every message for EN, are returned unchanged.
func Message(lang Lang, source string, args ...any) string {
	if text, ok := translations[lang][source]; ok {
		if len(args) > 0 {
			return fmt.Sprintf(text, args...)
		}
		return text
	}
	if len(args) > 0 {
		source = fmt.Sprintf(source, args...)
	}
	if text, ok := wrappedMessage(lang, source); ok {
		return text
	}
	return source
}

// wrappedMessage localizes source when it starts with a known wrapper prefix,
// keeping everything after that prefix untouched.
func wrappedMessage(lang Lang, source string) (string, bool) {
	table := wrapped[lang]
	longest := ""
	for prefix := range table {
		if len(prefix) > len(longest) && strings.HasPrefix(source, prefix) {
			longest = prefix
		}
	}
	if longest == "" {
		return "", false
	}
	return table[longest] + source[len(longest):], true
}

// FromRequest negotiates the response language of an admin request.
func FromRequest(r *http.Request) Lang {
	return FromHeader(r.Header.Get("Accept-Language"))
}

// FromHeader picks the best supported language from an Accept-Language header,
// honouring q-values and falling back to EN. Unsupported languages, and entries
// with q=0 (RFC 9110: "not acceptable"), never win.
func FromHeader(header string) Lang {
	best := EN
	bestQ := 0.0
	for _, item := range strings.Split(header, ",") {
		tag, q := parseAcceptLanguageItem(item)
		if q <= 0 {
			continue
		}
		lang, ok := matchLang(tag)
		if !ok {
			continue
		}
		// Strictly greater keeps the earliest entry on ties, and header order
		// is the client's own preference order.
		if q > bestQ {
			best, bestQ = lang, q
		}
	}
	return best
}

// parseAcceptLanguageItem splits one "tag;q=value" entry. A malformed q-value
// makes the entry unusable, which q=0 encodes.
func parseAcceptLanguageItem(item string) (string, float64) {
	parts := strings.Split(item, ";")
	tag := strings.ToLower(strings.TrimSpace(parts[0]))
	q := 1.0
	for _, param := range parts[1:] {
		param = strings.TrimSpace(param)
		if !strings.HasPrefix(param, "q=") && !strings.HasPrefix(param, "Q=") {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(param[2:]), 64)
		if err != nil {
			return tag, 0
		}
		q = value
	}
	return tag, q
}

// matchLang maps a language tag (or "*") to a supported language. Every zh
// variant resolves to Simplified Chinese, the only Chinese catalog we ship, so
// a zh-TW reader still gets Chinese rather than English.
func matchLang(tag string) (Lang, bool) {
	tag = strings.ReplaceAll(tag, "_", "-")
	switch {
	case tag == "*" || tag == "en" || strings.HasPrefix(tag, "en-"):
		return EN, true
	case tag == "zh" || strings.HasPrefix(tag, "zh-"):
		return ZHCN, true
	default:
		return EN, false
	}
}
