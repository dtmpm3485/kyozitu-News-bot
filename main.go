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
	minDelay      = time.Minute
	maxDelay      = 3 * time.Hour
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
	Day        string                 `json:"day"`
	Users      map[string]TrackedUser `json:"users"`
	NextPostAt int64                  `json:"next_post_at"`
	LastUserID string                 `json:"last_user_id,omitempty"`
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

func newStore(path string, loc *time.Location) (*Store, error) {
	s := &Store{
		path: path,
		data: PersistentState{Guilds: map[string]*GuildState{}},
		loc:  loc,
	}
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

func randomDelay() time.Duration {
	span := int64(maxDelay - minDelay)
	if span <= 0 {
		return minDelay
	}
	return minDelay + time.Duration(rand.Int63n(span+1))
}

func (s *Store) touch(guildID string, u TrackedUser, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	today := dayKey(now, s.loc)
	gs := s.data.Guilds[guildID]
	if gs == nil || gs.Day != today {
		gs = &GuildState{Day: today, Users: map[string]TrackedUser{}}
		s.data.Guilds[guildID] = gs
	}
	if gs.Users == nil {
		gs.Users = map[string]TrackedUser{}
	}
	gs.Users[u.UserID] = u
	if gs.NextPostAt == 0 {
		gs.NextPostAt = now.Add(randomDelay()).Unix()
	}
	return s.saveLocked()
}

type DueNews struct {
	GuildID string
	User    TrackedUser
}

func (s *Store) due(now time.Time) ([]DueNews, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	today := dayKey(now, s.loc)
	var due []DueNews
	dirty := false

	for guildID, gs := range s.data.Guilds {
		if gs.Day != today {
			gs.Day = today
			gs.Users = map[string]TrackedUser{}
			gs.NextPostAt = 0
			gs.LastUserID = ""
			dirty = true
			continue
		}
		if gs.NextPostAt == 0 || now.Unix() < gs.NextPostAt || len(gs.Users) == 0 {
			continue
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
		chosen := candidates[rand.Intn(len(candidates))]
		due = append(due, DueNews{GuildID: guildID, User: chosen})
		gs.LastUserID = chosen.UserID
		gs.NextPostAt = now.Add(randomDelay()).Unix()
		dirty = true
	}

	if dirty {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return due, nil
}

var timePhrases = []string{
	"本日", "本日未明", "本日午前", "本日午後", "昨夜", "先ほど", "つい先ほど", "今朝",
}

var incidents = []string{
	"自宅で盛大に脱糞した",
	"冷蔵庫を開けたまま中身と3分間にらみ合った",
	"国家機密に指定されたプリンを勝手に眺めた",
	"コンビニのおでんに無言で圧力をかけた",
	"Wi-Fiに対して謝罪を要求した",
	"午前3時に水を飲みすぎた",
	"階段を一段飛ばしで移動した",
	"唐揚げにレモンをかけるかどうかで国会を混乱させた",
	"猫とのじゃんけんに敗北した事実を隠蔽した",
	"布団から出るという公約を破った",
	"信号が青になる0.2秒前から歩く準備をした",
	"焼きそばの湯切りで近隣住民をざわつかせた",
	"サーバー内で『草』を必要以上に栽培した",
	"存在しない会議に遅刻した",
	"ラーメンの写真だけ見て満腹になったと主張した",
	"エレベーターの閉ボタンを2回押した",
	"目覚まし時計を止めたあと二度寝を決行した",
	"ポテトを1本だけ残すという不可解な行為に及んだ",
	"誰も聞いていないのに『知らんけど』で供述を締めた",
	"月面で駐車違反をした",
}

var punishments = []string{
	"現行犯逮捕されました",
	"逮捕され、事情聴取を受けることになりました",
	"無期懲役を言い渡されました",
	"懲役114514秒の判決を受けました",
	"処刑されましたが、3秒後に何事もなく復活しました",
	"無期休憩の処分となりました",
	"罰金3円を命じられました",
	"国外追放となり、隣のVCへ移送されました",
	"永久追放3分の処分を受けました",
	"死刑判決を受けましたが、担当者の寝坊により執行は中止されました",
	"厳重注意のうえ、おやつ抜き5分の処分となりました",
	"懲役0.7秒、執行猶予48年を言い渡されました",
}

var comments = []string{
	"本人は『記憶にございません』とコメントしています。",
	"本人は『大根が先に見てきた』と容疑を否認しています。",
	"関係者によると、現場は一時騒然としたような気がするとのことです。",
	"専門家は『かなりどうでもいい事件です』と分析しています。",
	"捜査関係者は『なぜこうなったのか我々にも分からない』としています。",
	"本人からのコメントは特に求められていません。",
	"近隣住民は『いつかやると思っていたような、思っていなかったような』と話しています。",
	"この件による実害は今のところ確認されていません。",
	"警察は余罪として二度寝の可能性も視野に調べています。",
	"なお、専門家は全員このニュースの存在を知りません。",
}

var sections = []string{"速報", "社会", "号外", "独自", "地方", "緊急", "謎"}

func pick(items []string) string {
	return items[rand.Intn(len(items))]
}

func buildNews(userID string) (string, string) {
	title := fmt.Sprintf("【虚実ニュース・%s】", pick(sections))
	body := fmt.Sprintf(
		"%s、<@%s> が%sことが判明し、%s。\n\n%s\n\n※このニュースは完全なフィクションです。実在の人物・事件・処分とは関係ありません。",
		pick(timePhrases), userID, pick(incidents), pick(punishments), pick(comments),
	)
	return title, body
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
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "lookup ") || strings.Contains(text, ":53")
}

func applyFallbackDNS(dg *discordgo.Session) {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			conn, err := d.DialContext(ctx, "udp", "1.1.1.1:53")
			if err == nil {
				return conn, nil
			}
			return d.DialContext(ctx, "udp", "8.8.8.8:53")
		},
	}

	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
		Resolver:  resolver,
	}

	dg.Client = &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         dialer.DialContext,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 15 * time.Second,
		},
		Timeout: 30 * time.Second,
	}
	dg.Dialer = &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		NetDialContext:   dialer.DialContext,
		HandshakeTimeout: 20 * time.Second,
	}
}

func main() {
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_BOT_TOKEN is required")
	}

	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		loc = time.FixedZone("JST", 9*60*60)
	}

	path, err := dataPath()
	if err != nil {
		log.Fatalf("failed to resolve data directory: %v", err)
	}
	store, err := newStore(path, loc)
	if err != nil {
		log.Fatalf("failed to load state: %v", err)
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("failed to create Discord session: %v", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuildMessages

	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.GuildID == "" || m.Author == nil || m.Author.Bot {
			return
		}
		display := m.Author.GlobalName
		if display == "" {
			display = m.Author.Username
		}
		u := TrackedUser{
			UserID:      m.Author.ID,
			Username:    m.Author.Username,
			DisplayName: display,
			ChannelID:   m.ChannelID,
			LastSeen:    time.Now().Unix(),
		}
		if err := store.touch(m.GuildID, u, time.Now()); err != nil {
			log.Printf("failed to save activity: %v", err)
		}
	})

	log.Printf("connecting to Discord...")
	if err := dg.Open(); err != nil {
		if isDNSError(err) {
			log.Printf("system DNS failed; retrying with fallback DNS (1.1.1.1 / 8.8.8.8)")
			applyFallbackDNS(dg)
			if retryErr := dg.Open(); retryErr != nil {
				log.Fatalf("failed to connect to Discord after DNS fallback: %v", retryErr)
			}
		} else {
			log.Fatalf("failed to connect to Discord: %v", err)
		}
	}
	defer dg.Close()

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
			if err != nil {
				log.Printf("scheduler error: %v", err)
				continue
			}
			for _, item := range items {
				title, body := buildNews(item.User.UserID)
				_, err := dg.ChannelMessageSendComplex(item.User.ChannelID, &discordgo.MessageSend{
					Content: title + "\n" + body,
					AllowedMentions: &discordgo.MessageAllowedMentions{
						Parse: []discordgo.AllowedMentionType{},
						Users: []string{item.User.UserID},
					},
				})
				if err != nil {
					log.Printf("failed to send news guild=%s channel=%s: %v", item.GuildID, item.User.ChannelID, err)
				}
			}
		case <-done:
			log.Printf("%s stopped", appName)
			return
		}
	}
}
