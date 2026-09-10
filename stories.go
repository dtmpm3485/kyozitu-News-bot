package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type storyTemplate struct {
	ID          string
	Category    string
	Headline    string
	Openings    []string
	Details     []string
	Witnesses   []string
	Org         string
	Punishments []string
	Aftermath   []string
	Endings     []string
}

type userStoryContext struct {
	Day           string `json:"day"`
	Role          string `json:"role"`
	LastStoryID   string `json:"last_story_id"`
	LastHeadline  string `json:"last_headline"`
	StoryCount    int    `json:"story_count"`
	LastPublished int64  `json:"last_published"`
}

type storyMemoryFile struct {
	Users map[string]*userStoryContext `json:"users"`
}

var (
	storyMu     sync.Mutex
	storyLoaded bool
	storyPath   string
	storyMemory = storyMemoryFile{Users: map[string]*userStoryContext{}}
)

var dailyRoles = []string{
	"全国プリン保安協会の臨時監査員",
	"布団離脱管理局の重点観測対象",
	"冷蔵庫外交評議会の交渉担当",
	"日本二度寝研究機構の特別研究員",
	"唐揚げ中立委員会の暫定議長",
	"Wi-Fi謝罪審査会の参考人",
	"月面交通安全センターの仮免許保持者",
	"全国ポテト一本残し対策本部の調査対象",
	"虚実庁・生活奇行分析室の協力者",
	"猫じゃんけん公正取引委員会の元審判",
	"おでん圧力監視機構の巡回員",
	"深夜ラーメン画像研究会の被験者",
	"エレベーター閉ボタン倫理委員会の委員",
	"焼きそば湯切り安全保障会議の証人",
	"サーバー草原化防止連盟の監査対象",
	"存在しない会議運営局の常任欠席者",
	"『知らんけど』言語文化保存会の広報代理",
	"階段一段飛ばし競技連盟の無登録選手",
	"自動販売機返事待ち協会の相談役",
	"虚実気象台・局地的気まずさ観測班の観測員",
}

var storyTemplates = allStoryTemplates()

func allStoryTemplates() []storyTemplate {
	all := make([]storyTemplate, 0, 20)
	all = append(all, storySet1()...)
	all = append(all, storySet2()...)
	all = append(all, storySet3()...)
	all = append(all, storySet4()...)
	return all
}

func richPick(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[rand.Intn(len(items))]
}

func renderStoryText(text string, vars map[string]string) string {
	for k, v := range vars {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return text
}

func categoryDesk(category string) string {
	switch category {
	case "社会":
		return "虚実ニュース社会部の取材で、新たな状況が分かりました。"
	case "経済":
		return "市場への実害は確認されていませんが、虚実経済部が動向を追っています。"
	case "科学":
		return "専門家は現象の再現性を慎重に調べています。"
	case "スポーツ":
		return "競技団体は記録の扱いについて確認を進めています。"
	case "国際":
		return "架空の関係機関が対応を協議しています。"
	case "文化":
		return "ネット文化への影響を含め、関係者が経緯を調べています。"
	case "気象":
		return "虚実気象台は局地的な現象として経過を観測しています。"
	case "交通":
		return "交通への大きな影響はありませんが、運用上の確認が行われています。"
	default:
		return "詳しい原因は分かっておらず、虚実ニュース取材班が調査を続けています。"
	}
}

func storyStateFile() string {
	base, err := dataPath()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(base), "story_state.json")
}

func loadStoryMemoryLocked() {
	if storyLoaded {
		return
	}
	storyLoaded = true
	storyPath = storyStateFile()
	if storyPath == "" {
		return
	}
	b, err := os.ReadFile(storyPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &storyMemory)
	if storyMemory.Users == nil {
		storyMemory.Users = map[string]*userStoryContext{}
	}
}

func saveStoryMemoryLocked() {
	if storyPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(storyPath), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(storyMemory, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(storyPath, b, 0o600)
}

func findStory(id string) *storyTemplate {
	for i := range storyTemplates {
		if storyTemplates[i].ID == id {
			return &storyTemplates[i]
		}
	}
	return nil
}

func chooseStory(ctx *userStoryContext) (*storyTemplate, bool) {
	if ctx.StoryCount > 0 && ctx.LastStoryID != "" && rand.Intn(100) < 35 {
		if st := findStory(ctx.LastStoryID); st != nil {
			return st, true
		}
	}
	if len(storyTemplates) == 1 {
		return &storyTemplates[0], false
	}
	for {
		st := &storyTemplates[rand.Intn(len(storyTemplates))]
		if st.ID != ctx.LastStoryID || rand.Intn(100) < 20 {
			return st, false
		}
	}
}

func buildRichNews(guildID, userID string) string {
	storyMu.Lock()
	defer storyMu.Unlock()
	loadStoryMemoryLocked()

	now := time.Now()
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		loc = time.FixedZone("JST", 9*60*60)
	}
	today := now.In(loc).Format("2006-01-02")
	for key, ctx := range storyMemory.Users {
		if ctx == nil || ctx.Day != today {
			delete(storyMemory.Users, key)
		}
	}

	key := guildID + ":" + userID
	ctx := storyMemory.Users[key]
	if ctx == nil {
		ctx = &userStoryContext{Day: today, Role: richPick(dailyRoles)}
		storyMemory.Users[key] = ctx
	}

	st, continuation := chooseStory(ctx)
	vars := map[string]string{
		"user":     userID,
		"clock":    fmt.Sprintf("%d時%02d分", rand.Intn(24), rand.Intn(60)),
		"minutes":  fmt.Sprintf("%d", 2+rand.Intn(47)),
		"count":    fmt.Sprintf("%d", 2+rand.Intn(18)),
		"count2":   fmt.Sprintf("%d", 2+rand.Intn(9)),
		"distance": fmt.Sprintf("%.1f", 0.3+rand.Float64()*9.4),
		"temp":     fmt.Sprintf("%d", 2+rand.Intn(12)),
		"yen":      fmt.Sprintf("%d", 1+rand.Intn(500)),
		"seconds":  fmt.Sprintf("%d", 1+rand.Intn(59)),
		"org":      st.Org,
		"role":     ctx.Role,
	}

	rarity := rand.Intn(100)
	label := st.Category
	if continuation {
		label = "続報"
	} else if ctx.StoryCount > 0 && rand.Intn(100) < 8 {
		label = "訂正"
	} else if rarity >= 95 {
		label = "特別速報"
	} else if rarity >= 70 {
		label = "独自"
	}

	headline := st.Headline
	parts := []string{fmt.Sprintf("【虚実ニュース・%s】%s", label, headline)}
	if label == "訂正" && ctx.LastHeadline != "" {
		parts = append(parts, fmt.Sprintf("先ほどお伝えした『%s』について、一部どうでもいい点を訂正します。", ctx.LastHeadline))
	} else if continuation && ctx.LastHeadline != "" {
		parts = append(parts, fmt.Sprintf("これまでにお伝えしている『%s』の続報です。", ctx.LastHeadline))
	}

	parts = append(parts, renderStoryText(richPick(st.Openings), vars))
	parts = append(parts, renderStoryText(richPick(st.Details), vars))
	parts = append(parts, categoryDesk(st.Category))
	parts = append(parts, renderStoryText(richPick(st.Witnesses), vars))

	if ctx.StoryCount == 0 {
		parts = append(parts, fmt.Sprintf("虚実庁の本日付の架空資料では、<@%s>は『%s』として登録されています。", userID, ctx.Role))
	} else if continuation || rand.Intn(100) < 45 {
		parts = append(parts, fmt.Sprintf("なお、<@%s>はきょうこれまで『%s』として扱われており、関係機関は過去の虚実ニュースとの関連も調べています。", userID, ctx.Role))
	}

	parts = append(parts, fmt.Sprintf("%sは対応として、%sを決定しました。", st.Org, renderStoryText(richPick(st.Punishments), vars)))
	parts = append(parts, renderStoryText(richPick(st.Aftermath), vars))
	if rarity >= 95 {
		parts = append(parts, "虚実ニュース取材班は現場周辺を約3分取材しましたが、追加で重要そうな事実は特に見つかりませんでした。")
	}
	parts = append(parts, renderStoryText(richPick(st.Endings), vars))
	parts = append(parts, "※このニュースは完全なフィクションです。実在の人物・事件・組織・処分とは関係ありません。")

	ctx.LastStoryID = st.ID
	ctx.LastHeadline = headline
	ctx.StoryCount++
	ctx.LastPublished = now.Unix()
	saveStoryMemoryLocked()
	return strings.Join(parts, "\n\n")
}
