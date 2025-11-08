package feishu

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/icza/gox/stringsx"
	"github.com/sirupsen/logrus"
	"github.com/xujiahua/alertmanager-webhook-feishu/config"
	"github.com/xujiahua/alertmanager-webhook-feishu/feishu/rotate"
	"github.com/xujiahua/alertmanager-webhook-feishu/model"
	"github.com/xujiahua/alertmanager-webhook-feishu/tmpl"
	"sort"
	"strings"
	"text/template"
	"time"
)

type Bot struct {
	webhook     string
	openIDs     []string
	rotator     *rotate.MentionRotator
	sdk         *Sdk
	tpl         *template.Template
	alertTpl    *template.Template
	titlePrefix string
	metadata    map[string]string
}

func New(bot *config.Bot, helper *EmailHelper) (*Bot, error) {
	// @xxx
	openIDs, err := getOpenIDs(bot.Mention, helper)
	if err != nil {
		return nil, err
	}

	var rotator *rotate.MentionRotator
	if bot.Mention != nil && bot.Mention.Rotation != "" && len(openIDs) > 1 {
		rotator, err = rotate.New(bot.Mention.Rotation, openIDs)
		if err != nil {
			return nil, err
		}
	}

	// template
	tpl, alertTpl, err := getTemplates(bot.Template)
	if err != nil {
		return nil, err
	}

	return &Bot{
		webhook:     bot.Webhook,
		rotator:     rotator,
		openIDs:     openIDs,
		sdk:         NewSDK("", ""),
		tpl:         tpl,
		alertTpl:    alertTpl,
		titlePrefix: bot.TitlePrefix,
		metadata:    bot.MetaData,
	}, nil
}

func getOpenIDs(mention *config.Mention, helper *EmailHelper) ([]string, error) {
	if mention == nil {
		return nil, nil
	}
	if mention.All {
		return []string{"all"}, nil
	}

	openIDs := mention.OpenIDs
	emails := mention.Emails
	if len(emails) != 0 && helper == nil {
		return nil, errors.New("@somebody by email need email flag enabled")
	}
	if len(emails) != 0 {
		remaining, err := helper.Lookup(emails)
		if err != nil {
			return nil, err
		}
		openIDs = append(openIDs, remaining...)
	}
	return openIDs, nil
}

func getTemplates(tmplConf *config.Template) (*template.Template, *template.Template, error) {
	if tmplConf != nil && tmplConf.CustomPath != "" {
		t, err := tmpl.GetCustomTemplate(tmplConf.CustomPath)
		if err != nil {
			return nil, nil, err
		}
		return t, nil, nil
	}

	// by default, use two tmpls, one is for alert
	dt, err := tmpl.GetEmbedTemplate("default.tmpl")
	if err != nil {
		return nil, nil, err
	}

	dat, err := tmpl.GetEmbedTemplate("default_alert.tmpl")
	if err != nil {
		return nil, nil, err
	}

	return dt, dat, nil
}

func (b Bot) Send(alerts *model.WebhookMessage) error {
	extractUniqueValues(alerts)
	if len(alerts.OpenIDs) == 0 {
		// attach @xxx
		if b.rotator != nil {
			alerts.OpenIDs = b.rotator.Rotate(time.Now())
		} else {
			alerts.OpenIDs = b.openIDs
		}
	}

	processAlertMessage(alerts)

	// title prefix
	//alerts.TitlePrefix = b.titlePrefix

	// merge metadata
	//alerts.Meta = mergeMap(alerts.Meta, b.metadata)

	//err := b.preprocessAlerts(alerts)
	//if err != nil {
	//	return err
	//}

	marshal, _ := json.Marshal(alerts)
	logrus.Infof("request feishu body: %s\n", string(marshal))
	var buf bytes.Buffer
	err := b.tpl.Execute(&buf, alerts)
	if err != nil {
		return err
	}
	if logrus.IsLevelEnabled(logrus.InfoLevel) {
		if d, err := beautifyJSON(buf.String()); err != nil {
			logrus.Error(err)
			logrus.Infoln(buf.String())
		} else {
			logrus.Infoln(d)
		}
	}

	if webHookToken, ok := alerts.CommonAnnotations["webHookToken"]; ok {
		b.webhook = "https://open.feishu.cn/open-apis/bot/v2/hook/" + webHookToken
	}

	return b.sdk.WebhookV2(b.webhook, &buf)
}

func extractUniqueValues(alerts *model.WebhookMessage) {
	alerts.OpenIDs = []string{}
	alerts.Teams = []string{}

	openIDSet := make(map[string]bool)
	teamSet := make(map[string]bool)

	for _, alert := range alerts.Alerts.Firing() {
		extractFromAnnotation(alert.Annotations, "openIds", openIDSet)
		extractFromAnnotation(alert.Annotations, "teams", teamSet)
	}

	alerts.OpenIDs = mapToSlice(openIDSet)
	alerts.Teams = mapToSlice(teamSet)
}

func extractFromAnnotation(annotations map[string]string, key string, targetSet map[string]bool) {
	if value, ok := annotations[key]; ok && value != "" {
		values := strings.Split(value, ",")
		for _, v := range values {
			v = strings.TrimSpace(v)
			if v != "" {
				targetSet[v] = true
			}
		}
	}
}

func mapToSlice(stringSet map[string]bool) []string {
	result := make([]string, 0, len(stringSet))
	for k := range stringSet {
		result = append(result, k)
	}
	sort.Strings(result)
	return result
}

func processAlertMessage(message *model.WebhookMessage) {
	// 设置默认值
	message.Level = "未知"
	message.SubTitle = message.GroupLabels["alertname"]
	if message.SubTitle == "" {
		message.SubTitle = "告警通知"
	}

	alerts := message.Alerts
	if len(alerts) == 0 {
		return
	}

	// 单个告警时直接使用第一个告警的信息
	if len(alerts) == 1 {
		alert := alerts[0]

		if level, exists := alert.Annotations["level"]; exists && level != "" {
			message.Level = level
		} else if severity, exists := alert.Annotations["severity"]; exists && severity != "" {
			message.Level = severity
		}

		// 设置标题
		if title, exists := alert.Annotations["title"]; exists && title != "" {
			message.SubTitle = title
		}

		if link, exists := alert.Annotations["link"]; exists && link != "" {
			message.Link = link
		}

		if buttonName, exists := alert.Annotations["buttonName"]; exists && buttonName != "" {
			message.ButtonName = buttonName
		}
		return
	}

	// 多个告警时按优先级查找
	priorityLevels := []string{"P0", "P1", "P2", "P3", "P4"}

	for _, priority := range priorityLevels {
		for _, alert := range alerts {
			if level, exists := alert.Annotations["level"]; exists && level == priority {
				// 找到匹配的优先级，设置等级和标题
				message.Level = level
				if title, exists := alert.Annotations["title"]; exists && title != "" {
					message.SubTitle = title
				}
				if link, exists := alert.Annotations["link"]; exists && link != "" {
					message.Link = link
				}
				if buttonName, exists := alert.Annotations["buttonName"]; exists && buttonName != "" {
					message.ButtonName = buttonName
				}
				return
			}
		}
	}

	// 如果没有找到优先级等级，使用第一个告警的信息
	firstAlert := alerts[0]
	if level, exists := firstAlert.Annotations["level"]; exists && level != "" {
		message.Level = level
	} else if severity, exists := firstAlert.Annotations["severity"]; exists && severity != "" {
		message.Level = severity
	}

	if title, exists := firstAlert.Annotations["title"]; exists && title != "" {
		message.SubTitle = title
	}
	if link, exists := firstAlert.Annotations["link"]; exists && link != "" {
		message.Link = link
	}
	if buttonName, exists := firstAlert.Annotations["buttonName"]; exists && buttonName != "" {
		message.ButtonName = buttonName
	}
}

// right is immutable
func mergeMap(left, right map[string]string) map[string]string {
	if len(right) == 0 {
		return left
	}
	if left == nil {
		left = make(map[string]string)
	}
	for k, v := range right {
		if _, ok := left[k]; !ok {
			left[k] = v
		}
	}
	return left
}

// field description may contain double quote, non printable chars
func fixDescription(s string) string {
	// feishu fix: clean non printable char
	s = stringsx.Clean(s)
	// feishu fix: unescape a string
	s = fmt.Sprintf("%#v", s)
	// remove prefix and suffix double quote, means we just unescape inner text
	s = strings.TrimPrefix(s, "\"")
	s = strings.TrimSuffix(s, "\"")
	return s
}

func (b Bot) preprocessAlerts(alerts *model.WebhookMessage) error {
	if b.alertTpl == nil {
		return nil
	}

	// preprocess using alert template
	for _, alert := range alerts.Alerts.Firing() {
		var buf bytes.Buffer
		if _, ok := alert.Annotations["description"]; ok {
			alert.Annotations["description"] = fixDescription(alert.Annotations["description"])
		}
		err := b.alertTpl.Execute(&buf, alert)
		if err != nil {
			return err
		}
		res := strings.ReplaceAll(buf.String(), "\n", "\\n")
		alerts.FiringAlerts = append(alerts.FiringAlerts, res)
	}
	for _, alert := range alerts.Alerts.Resolved() {
		var buf bytes.Buffer
		if _, ok := alert.Annotations["description"]; ok {
			alert.Annotations["description"] = fixDescription(alert.Annotations["description"])
		}
		err := b.alertTpl.Execute(&buf, alert)
		if err != nil {
			return err
		}
		res := strings.ReplaceAll(buf.String(), "\n", "\\n")
		alerts.ResolvedAlerts = append(alerts.ResolvedAlerts, res)
	}

	return nil
}

func beautifyJSON(raw string) (string, error) {
	data := make(map[string]interface{})
	err := json.Unmarshal([]byte(raw), &data)
	if err != nil {
		return "", err
	}
	d, err := json.MarshalIndent(data, "", "\t")
	if err != nil {
		return "", err
	}
	return string(d), nil
}
