package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

const (
	appName       = "虚実ニュースbot"
	defaultMin    = time.Minute
	defaultMax    = 3 * time.Hour
	schedulerTick = 10 * time.Second
)

type TrackedUser struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	ChannelID   string `json:"channel_id"`
	LastSeen    int64  `json:"last_seen"`
}

type GuildState struct {
	Day            string                 `json:"day"`
	Users          map[string]TrackedUser `json:"users"`
	NextPostAt     int64                  `json:"next_post_at"`
	LastUserID     string                 `json:"last_user_id,omitempty"`
	Disabled       bool                   `json:"disabled,omitempty"`
	FixedChannelID string                 `json:"fixed_channel_id,omitempty"`
	MinDelaySec    int64                  `json:"min_delay_sec,omitempty"`
	MaxDelaySec    int64                  `json:"max_delay_sec,omitempty"`
}

type PersistentState struct {
	Guilds map[string]*GuildState `json:"guilds"`
}

type Store struct {
	mu   sync.Mutex
	path string
	data PersistentState
	loc  *time.Location
}

type GuildStatus struct {
	Enabled        bool
	CandidateCount int
	NextPostAt     int64
	FixedChannelID string
	MinDelay       time.Duration
	MaxDelay       time.Duration
}

type DueNews struct {
	GuildID   string
	ChannelID string
	User      TrackedUser
}

func newStore(path string, loc *time.Location) (*Store, error) {
	s := &Store{path: path, data: PersistentState{Guilds: map[string]*GuildState{}}, loc: loc}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return fmt.Errorf("state file is invalid: %w", err)
	}
	if s.data.Guilds == nil {
		s.data.Guilds = map[string]*GuildState{}
	}
	return nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

func dayKey(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02")
}

func effectiveDelays(gs *GuildState) (time.Duration, time.Duration) {
	minD, maxD := defaultMin, defaultMax
	if gs.MinDelaySec > 0 {
		minD = time.Duration(gs.MinDelaySec) * time.Second
	}
	if gs.MaxDelaySec > 0 {
		maxD = time.Duration(gs.MaxDelaySec) * time.Second
	}
	if maxD < minD {
		maxD = minD
	}
	return minD, maxD
}

func randomDelayFor(gs *GuildState) time.Duration {
	minD, maxD := effectiveDelays(gs)
	span := int64(maxD - minD)
	if span <= 0 {
		return minD
	}
	return minD + time.Duration(rand.Int63n(span+1))
}

func (s *Store) ensureGuildLocked(guildID string, now time.Time) *GuildState {
	today := dayKey(now, s.loc)
	gs := s.data.Guilds[guildID]
	if gs == nil {
		gs = &GuildState{Day: today, Users: map[string]TrackedUser{}}
		s.data.Guilds[guildID] = gs
	}
	if gs.Users == nil {
		gs.Users = map[string]TrackedUser{}
	}
	if gs.Day != today {
		gs.Day = today
		gs.Users = map[string]TrackedUser{}
		gs.NextPostAt = 0
		gs.LastUserID = ""
	}
	return gs
}

func (s *Store) touch(guildID string, u TrackedUser, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	gs := s.ensureGuildLocked(guildID, now)
	gs.Users[u.UserID] = u
	if !gs.Disabled && gs.NextPostAt == 0 {
		gs.NextPostAt = now.Add(randomDelayFor(gs)).Unix()
	}
	return s.saveLocked()
}

func chooseUser(gs *GuildState) (TrackedUser, bool) {
	if len(gs.Users) == 0 {
		return TrackedUser{}, false
	}
	candidates := make([]TrackedUser, 0, len(gs.Users))
	for _, u := range gs.Users {
		if len(gs.Users) > 1 && u.UserID == gs.LastUserID {
			continue
		}
		candidates = append(candidates, u)
	}
	if len(candidates) == 0 {
		for _, u := range gs.Users {
			candidates = append(candidates, u)
		}
	}
	return candidates[rand.Intn(len(candidates))], true
}

func (s *Store) due(now time.Time) ([]DueNews, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []DueNews
	dirty := false
	for guildID := range s.data.Guilds {
		gs := s.ensureGuildLocked(guildID, now)
		if gs.Disabled || gs.NextPostAt == 0 || now.Unix() < gs.NextPostAt || len(gs.Users) == 0 {
			continue
		}
		chosen, ok := chooseUser(gs)
		if !ok {
			continue
		}
		channelID := gs.FixedChannelID
		if channelID == "" {
			channelID = chosen.ChannelID
		}
		due = append(due, DueNews{GuildID: guildID, ChannelID: channelID, User: chosen})
		gs.LastUserID = chosen.UserID
		gs.NextPostAt = now.Add(randomDelayFor(gs)).Unix()
		dirty = true
	}
	if dirty {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return due, nil
}

func (s *Store) setEnabled(guildID string, enabled bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	gs := s.ensureGuildLocked(guildID, now)
	gs.Disabled = !enabled
	if enabled {
		if len(gs.Users) > 0 {
			gs.NextPostAt = now.Add(randomDelayFor(gs)).Unix()
		}
	} else {
		gs.NextPostAt = 0
	}
	return s.saveLocked()
}

func (s *Store) setChannel(guildID, channelID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	gs := s.ensureGuildLocked(guildID, now)
	gs.FixedChannelID = channelID
	return s.saveLocked()
}

func (s *Store) setInterval(guildID string, minMinutes, maxMinutes int64, now time.Time) error {
	if minMinutes < 1 || maxMinutes < 1 || minMinutes > maxMinutes || maxMinutes > 1440 {
		return fmt.Errorf("interval must be 1-1440 minutes and min must be <= max")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gs := s.ensureGuildLocked(guildID, now)
	gs.MinDelaySec = minMinutes * 60
	gs.MaxDelaySec = maxMinutes * 60
	if !gs.Disabled && len(gs.Users) > 0 {
		gs.NextPostAt = now.Add(randomDelayFor(gs)).Unix()
	}
	return s.saveLocked()
}

func (s *Store) resetUsers(guildID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	gs := s.ensureGuildLocked(guildID, now)
	gs.Users = map[string]TrackedUser{}
	gs.LastUserID = ""
	gs.NextPostAt = 0
	return s.saveLocked()
}

func (s *Store) status(guildID string, now time.Time) GuildStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	gs := s.ensureGuildLocked(guildID, now)
	minD, maxD := effectiveDelays(gs)
	return GuildStatus{Enabled: !gs.Disabled, CandidateCount: len(gs.Users), NextPostAt: gs.NextPostAt, FixedChannelID: gs.FixedChannelID, MinDelay: minD, MaxDelay: maxD}
}

func (s *Store) testUser(guildID string, now time.Time) (TrackedUser, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gs := s.ensureGuildLocked(guildID, now)
	u, ok := chooseUser(gs)
	if !ok {
		return TrackedUser{}, "", false
	}
	channelID := gs.FixedChannelID
	if channelID == "" {
		channelID = u.ChannelID
	}
	return u, channelID, true
}

var timePhrases = []string{"本日", "本日未明", "本日午前", "本日午後", "昨夜", "先ほど", "つい先ほど", "今朝"}
var incidents = []string{
	"自宅で盛大に脱糞した", "冷蔵庫を開けたまま中身と3分間にらみ合った", "国家機密に指定されたプリンを勝手に眺めた",
	"コンビニのおでんに無言で圧力をかけた", "Wi-Fiに対して謝罪を要求した", "午前3時に水を飲みすぎた",
	"階段を一段飛ばしで移動した", "唐揚げにレモンをかけるかどうかで国会を混乱させた", "猫とのじゃんけんに敗北した事実を隠蔽した",
	"布団から出るという公約を破った", "信号が青になる0.2秒前から歩く準備をした", "焼きそばの湯切りで近隣住民をざわつかせた",
	"サーバー内で『草』を必要以上に栽培した", "存在しない会議に遅刻した", "ラーメンの写真だけ見て満腹になったと主張した",
	"エレベーターの閉ボタンを2回押した", "目覚まし時計を止めたあと二度寝を決行した", "ポテトを1本だけ残すという不可解な行為に及んだ",
	"誰も聞いていないのに『知らんけど』で供述を締めた", "月面で駐車違反をした",
}
var punishments = []string{
	"現行犯逮捕されました", "逮捕され、事情聴取を受けることになりました", "無期懲役を言い渡されました", "懲役114514秒の判決を受けました",
	"処刑されましたが、3秒後に何事もなく復活しました", "無期休憩の処分となりました", "罰金3円を命じられました", "国外追放となり、隣のVCへ移送されました",
	"永久追放3分の処分を受けました", "死刑判決を受けましたが、担当者の寝坊により執行は中止されました", "厳重注意のうえ、おやつ抜き5分の処分となりました",
	"懲役0.7秒、執行猶予48年を言い渡されました",
}
var comments = []string{
	"本人は『記憶にございません』とコメントしています。", "本人は『大根が先に見てきた』と容疑を否認しています。",
	"関係者によると、現場は一時騒然としたような気がするとのことです。", "専門家は『かなりどうでもいい事件です』と分析しています。",
	"捜査関係者は『なぜこうなったのか我々にも分からない』としています。", "本人からのコメントは特に求められていません。",
	"近隣住民は『いつかやると思っていたような、思っていなかったような』と話しています。", "この件による実害は今のところ確認されていません。",
	"警察は余罪として二度寝の可能性も視野に調べています。", "なお、専門家は全員このニュースの存在を知りません。",
}
var sections = []string{"速報", "社会", "号外", "独自", "地方", "緊急", "謎"}

func pick(items []string) string { return items[rand.Intn(len(items))] }

func buildNews(userID string) string {
	return fmt.Sprintf("【虚実ニュース・%s】\n%s、<@%s> が%sことが判明し、%s。\n\n%s\n\n※このニュースは完全なフィクションです。実在の人物・事件・処分とは関係ありません。",
		pick(sections), pick(timePhrases), userID, pick(incidents), pick(punishments), pick(comments))
}

func dataPath() (string, error) {
	if dir := os.Getenv("KYOZITU_DATA_DIR"); dir != "" {
		return filepath.Join(dir, "state.json"), nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "kyozitu-news-bot", "state.json"), nil
}

func isDNSError(err error) bool {
	if err == nil { return false }
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) { return true }
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "lookup ") || strings.Contains(text, ":53")
}

func applyFallbackDNS(dg *discordgo.Session) {
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		conn, err := d.DialContext(ctx, "udp", "1.1.1.1:53")
		if err == nil { return conn, nil }
		return d.DialContext(ctx, "udp", "8.8.8.8:53")
	}}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Resolver: resolver}
	dg.Client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second}, Timeout: 30 * time.Second}
	dg.Dialer = &websocket.Dialer{Proxy: http.ProxyFromEnvironment, NetDialContext: dialer.DialContext, HandshakeTimeout: 20 * time.Second}
}

func newsCommand() *discordgo.ApplicationCommand {
	perm := int64(discordgo.PermissionManageGuild)
	return &discordgo.ApplicationCommand{
		Name: "news", Description: "虚実ニュースbotの設定", DefaultMemberPermissions: &perm,
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "test", Description: "今すぐテストニュースを1件投稿"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "on", Description: "自動投稿をON"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "off", Description: "自動投稿をOFF"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status", Description: "現在の設定と次回投稿予定を表示"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "channel", Description: "投稿先チャンネルを固定", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionChannel, Name: "target", Description: "投稿先。省略時は現在のチャンネル", Required: false}}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "interval", Description: "ランダム投稿間隔を変更", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionInteger, Name: "min", Description: "最短（分）", Required: true}, {Type: discordgo.ApplicationCommandOptionInteger, Name: "max", Description: "最長（分）", Required: true}}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "reset", Description: "今日の候補ユーザーをリセット"},
		},
	}
}

func ephemeral(s *discordgo.Session, i *discordgo.InteractionCreate, text string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral}})
}

func optionMap(options []*discordgo.ApplicationCommandInteractionDataOption) map[string]*discordgo.ApplicationCommandInteractionDataOption {
	m := make(map[string]*discordgo.ApplicationCommandInteractionDataOption, len(options))
	for _, o := range options { m[o.Name] = o }
	return m
}

func handleNewsCommand(store *Store, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.GuildID == "" || i.Member == nil { ephemeral(s, i, "このコマンドはサーバー内で使ってください。"); return }
	if i.Member.Permissions&discordgo.PermissionManageGuild == 0 && i.Member.Permissions&discordgo.PermissionAdministrator == 0 {
		ephemeral(s, i, "サーバー管理権限が必要です。")
		return
	}
	data := i.ApplicationCommandData()
	if data.Name != "news" || len(data.Options) == 0 { return }
	sub := data.Options[0]
	now := time.Now()

	switch sub.Name {
	case "on":
		if err := store.setEnabled(i.GuildID, true, now); err != nil { ephemeral(s, i, "設定保存に失敗しました: "+err.Error()); return }
		ephemeral(s, i, "虚実ニュースの自動投稿をONにしました。")
	case "off":
		if err := store.setEnabled(i.GuildID, false, now); err != nil { ephemeral(s, i, "設定保存に失敗しました: "+err.Error()); return }
		ephemeral(s, i, "虚実ニュースの自動投稿をOFFにしました。")
	case "status":
		st := store.status(i.GuildID, now)
		next := "未定（候補ユーザーの発言待ち）"
		if st.NextPostAt > 0 { next = fmt.Sprintf("<t:%d:F>（<t:%d:R>）", st.NextPostAt, st.NextPostAt) }
		channel := "自動（選ばれたユーザーが最後に話したチャンネル）"
		if st.FixedChannelID != "" { channel = "<#" + st.FixedChannelID + ">" }
		ephemeral(s, i, fmt.Sprintf("自動投稿: %s\n候補ユーザー: %d人\n投稿間隔: %d〜%d分\n投稿先: %s\n次回: %s", map[bool]string{true:"ON", false:"OFF"}[st.Enabled], st.CandidateCount, int(st.MinDelay.Minutes()), int(st.MaxDelay.Minutes()), channel, next))
	case "channel":
		channelID := i.ChannelID
		m := optionMap(sub.Options)
		if o, ok := m["target"]; ok { channelID = o.StringValue() }
		if err := store.setChannel(i.GuildID, channelID, now); err != nil { ephemeral(s, i, "設定保存に失敗しました: "+err.Error()); return }
		ephemeral(s, i, "投稿先を <#"+channelID+"> に固定しました。")
	case "interval":
		m := optionMap(sub.Options)
		minOpt, ok1 := m["min"]; maxOpt, ok2 := m["max"]
		if !ok1 || !ok2 { ephemeral(s, i, "min と max を指定してください。"); return }
		minM, maxM := minOpt.IntValue(), maxOpt.IntValue()
		if err := store.setInterval(i.GuildID, minM, maxM, now); err != nil { ephemeral(s, i, "1〜1440分の範囲で、min <= max にしてください。"); return }
		ephemeral(s, i, fmt.Sprintf("投稿間隔を %d〜%d分 に変更しました。次回時刻も再抽選します。", minM, maxM))
	case "reset":
		if err := store.resetUsers(i.GuildID, now); err != nil { ephemeral(s, i, "リセットに失敗しました: "+err.Error()); return }
		ephemeral(s, i, "今日の候補ユーザーをリセットしました。")
	case "test":
		u, channelID, ok := store.testUser(i.GuildID, now)
		if !ok { ephemeral(s, i, "まだ候補ユーザーがいません。誰かが一度発言してから試してください。"); return }
		if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Content: buildNews(u.UserID), AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{u.UserID}}}); err != nil {
			ephemeral(s, i, "投稿に失敗しました: "+err.Error()); return
		}
		ephemeral(s, i, "テストニュースを投稿しました。")
	}
}

func registerGuildCommand(s *discordgo.Session, guildID string) {
	if s.State == nil || s.State.User == nil || guildID == "" { return }
	if _, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, guildID, []*discordgo.ApplicationCommand{newsCommand()}); err != nil {
		log.Printf("failed to register /news for guild=%s: %v", guildID, err)
	}
}

func main() {
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" { log.Fatal("DISCORD_BOT_TOKEN is required") }
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil { loc = time.FixedZone("JST", 9*60*60) }
	path, err := dataPath()
	if err != nil { log.Fatalf("failed to resolve data directory: %v", err) }
	store, err := newStore(path, loc)
	if err != nil { log.Fatalf("failed to load state: %v", err) }

	dg, err := discordgo.New("Bot " + token)
	if err != nil { log.Fatalf("failed to create Discord session: %v", err) }
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages

	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.GuildID == "" || m.Author == nil || m.Author.Bot { return }
		display := m.Author.GlobalName
		if display == "" { display = m.Author.Username }
		u := TrackedUser{UserID: m.Author.ID, Username: m.Author.Username, DisplayName: display, ChannelID: m.ChannelID, LastSeen: time.Now().Unix()}
		if err := store.touch(m.GuildID, u, time.Now()); err != nil { log.Printf("failed to save activity: %v", err) }
	})
	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) { handleNewsCommand(store, s, i) })
	dg.AddHandler(func(s *discordgo.Session, g *discordgo.GuildCreate) { registerGuildCommand(s, g.ID) })

	log.Printf("connecting to Discord...")
	if err := dg.Open(); err != nil {
		if isDNSError(err) {
			log.Printf("system DNS failed; retrying with fallback DNS (1.1.1.1 / 8.8.8.8)")
			applyFallbackDNS(dg)
			if retryErr := dg.Open(); retryErr != nil { log.Fatalf("failed to connect to Discord after DNS fallback: %v", retryErr) }
		} else { log.Fatalf("failed to connect to Discord: %v", err) }
	}
	defer dg.Close()

	for _, g := range dg.State.Guilds { registerGuildCommand(dg, g.ID) }
	log.Printf("%s started", appName)
	log.Printf("state: %s", path)

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()

	for {
		select {
		case now := <-ticker.C:
			items, err := store.due(now)
			if err != nil { log.Printf("scheduler error: %v", err); continue }
			for _, item := range items {
				_, err := dg.ChannelMessageSendComplex(item.ChannelID, &discordgo.MessageSend{Content: buildNews(item.User.UserID), AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{item.User.UserID}}})
				if err != nil { log.Printf("failed to send news guild=%s channel=%s: %v", item.GuildID, item.ChannelID, err) }
			}
		case <-done:
			log.Printf("%s stopped", appName)
			return
		}
	}
}
