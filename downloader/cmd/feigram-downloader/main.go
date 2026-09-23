package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"rsc.io/qr"
)

const (
	defaultPartSize = 1024 * 1024

	// R4.25：原生下载无进度看门狗窗口。健康下载每秒都有字节流动；
	// 120 秒零字节意味着媒体路径已挂死（DC 路由异常/代理不放行媒体段/对端无响应），
	// 必须转为可诊断的瞬态错误，而不是 goroutine 永久悬挂（2.6.2 实测队列冻死的根因）。
	nativeNoProgressTimeout = 120 * time.Second

	// R4.28：首字节超时——本次尝试一个字节都没收到时，30 秒即快速失败。
	// 媒体路径不通（代理不放行媒体 DC 段）时每次尝试都 0 字节，120s 常规窗口
	// 会让单并发队列被一个任务白占 2 分钟；30s 档加快轮换与诊断反馈。
	nativeFirstByteTimeout = 30 * time.Second

	// R4.29：后台缓存任务两次调度之间的最小间隔（错峰），避免缓存批量入队时
	// 短时间内连续建连/exportAuth 加深账号限流。
	autoSpawnMinInterval = 2 * time.Second
)

// version 是 Go 下载器对外上报的版本号。
// R4.25：改为可变并由构建注入 —— 打包脚本用
// `-ldflags "-X main.version=${VERSION}"` 写入 FPK 包版本，使日志首行
// 「Feigram Downloader <版本> listening」与安装包版本一致。此前它是写死的
// 独立常量（0.8.x），用户日志里恒为旧值，无法证明 NAS 上跑的是哪个二进制，
// 2.6.2 排障时因此浪费过一轮假设。未注入时（本地 go run / go test）回落到 dev。
var version = "dev"

// errDownloadStalled 表示下载长时间零进度。它是瞬态错误：按退避自动续传，
// 且错误文案必须直指「媒体路径无进度」这一事实，让用户能区分「慢」与「挂死」。
var errDownloadStalled = errors.New("下载超过 120 秒没有任何进度（媒体路径疑似挂死：请检查代理是否放行 Telegram 媒体 DC 网段），将自动重试")

// errEmptyMediaResponse 表示本次尝试一个有效分块都没拿到（R4.26）。
// 它是瞬态错误：链路/账号层故障的典型表现（配合看门狗 stalled 成对出现），
// 按退避自动续传，而不是终态失败后被外部反复复活。
var errEmptyMediaResponse = errors.New("媒体源返回空响应")

// errMissingFilePathReason 是任务缺少落盘路径时的等待原因文案。
// R4.25：此前 FilePath 为空直接在调度循环里 continue（零日志零提示），
// 用户在下载中心只能看到永久「排队中」——补上可读原因，避免二次排障盲区。
const errMissingFilePathReason = "任务缺少落盘路径，等待重新创建下载任务"

var migrateRe = regexp.MustCompile(`(?:FILE|PHONE|NETWORK|USER)?_?MIGRATE_([0-9]+)`)

// M5.2：结构化日志（级别/账号/任务维度）。
// 默认 JSON 输出到 stdout，级别由 LOG_LEVEL 环境变量控制（debug/info/warn/error，默认 info）。
var appLog = newAppLogger()

func newAppLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// taskLog 返回携带 task/account 维度的结构化日志器，便于按任务与账号检索。
func taskLog(taskID, account string) *slog.Logger {
	return appLog.With(slog.String("task", taskID), slog.String("account", account))
}

// storeSchemaVersion 是 Go 下载服务自有数据 schema 的当前版本，用于健康/状态接口暴露。
const storeSchemaVersion = 1

type Config struct {
	Enabled      bool   `json:"enabled"`
	Concurrency  int    `json:"concurrency"`
	RateLimitBps int64  `json:"rateLimitBps"`
	Mode         string `json:"mode"`
	PartSize     int64  `json:"partSize"`
	Backend      string `json:"backend"`
	Transport    string `json:"transport"`
	// ProxyURL 是应用内配置的代理地址（socks5://、socks5h://、http://、https://）。
	// 为空时回落到环境变量（FEIGRAM_PROXY_URL / ALL_PROXY / HTTPS_PROXY / ...），
	// 两者都没有则直连。详见 proxy.go。
	ProxyURL  string `json:"proxyUrl"`
	UpdatedAt string `json:"updatedAt"`
}

type Task struct {
	ID          string             `json:"id"`
	UserID      string             `json:"userId"`
	AccountID   string             `json:"accountId"`
	PeerID      string             `json:"peerId"`
	MessageID   int64              `json:"messageId"`
	FileName    string             `json:"fileName"`
	ContentType string             `json:"contentType"`
	Kind        string             `json:"kind"`
	Size        int64              `json:"size"`
	Downloaded  int64              `json:"downloaded"`
	SpeedBps    int64              `json:"speedBps"`
	Status      string             `json:"status"`
	Source      string             `json:"source"`
	AutoCache   bool               `json:"autoCache"`
	Transport   string             `json:"transport"`
	SourceURL   string             `json:"sourceUrl"`
	FilePath    string             `json:"filePath"`
	PartPath    string             `json:"partPath"`
	InlineURL   string             `json:"inlineUrl"`
	NativeFile  NativeFileLocation `json:"nativeFile"`
	NativePeer  NativePeerLocation `json:"nativePeer"`
	Error       string             `json:"error"`
	RetryCount  int                `json:"retryCount"`
	RetryAfter  int64              `json:"retryAfterUnix"`
	Order       int64              `json:"order"`
	CreatedAt   string             `json:"createdAt"`
	UpdatedAt   string             `json:"updatedAt"`
}

type NativeFileLocation struct {
	PeerID        string `json:"peerId"`
	MessageID     int64  `json:"messageId"`
	Kind          string `json:"kind"`
	FileID        string `json:"fileId"`
	AccessHash    string `json:"accessHash"`
	FileReference string `json:"fileReference"`
	DCID          int    `json:"dcId"`
	Size          int64  `json:"size"`
	MimeType      string `json:"mimeType"`
	FileName      string `json:"fileName"`
	UpdatedAt     string `json:"updatedAt"`
}

type NativePeerLocation struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	AccessHash string `json:"accessHash"`
}

type NativeAccount struct {
	UserID              string `json:"userId"`
	AccountID           string `json:"accountId"`
	Phone               string `json:"phone"`
	DisplayName         string `json:"displayName"`
	APIID               int    `json:"apiId"`
	APIHash             string `json:"apiHash"`
	Status              string `json:"status"`
	Ready               bool   `json:"ready"`
	Session             string `json:"session"`
	Error               string `json:"error"`
	HealthPasses        int    `json:"healthPasses"`
	LastSuccessAt       string `json:"lastSuccessAt"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	CreatedAt           string `json:"createdAt"`
	UpdatedAt           string `json:"updatedAt"`
	CheckedAt           string `json:"checkedAt"`
}

type NativeLogin struct {
	ID            string
	UserID        string
	AccountID     string
	Phone         string
	APIID         int
	APIHash       string
	CodeHash      string
	Status        string
	NeedsPassword bool
	Code          chan string
	Password      chan string
	Result        chan nativeLoginResult
	StartResult   chan nativeLoginResult
	Cancel        context.CancelFunc
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type NativeQRLogin struct {
	ID        string
	UserID    string
	AccountID string
	APIID     int
	APIHash   string
	Token     []byte
	URL       string
	QRImage   string
	Status    string
	Error     string
	Polling   bool
	Expires   time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

type nativeLoginResult struct {
	Account          NativeAccount
	Error            error
	LoginID          string
	PhoneCodeHash    string
	PasswordRequired bool
	Done             bool
}

type nativeQRLoginResult struct {
	Account NativeAccount `json:"account,omitempty"`
	LoginID string        `json:"loginId"`
	URL     string        `json:"url,omitempty"`
	QRImage string        `json:"qrImage,omitempty"`
	Status  string        `json:"status"`
	Done    bool          `json:"done"`
	Error   string        `json:"error,omitempty"`
	Expires string        `json:"expires,omitempty"`
}

type Store struct {
	Config Config `json:"config"`
	Tasks  []Task `json:"tasks"`
	Meta   Meta   `json:"meta"`
}

type NativeStore struct {
	Accounts []NativeAccount `json:"accounts"`
}

type Meta struct {
	StartedAt string `json:"startedAt"`
	PID       int    `json:"pid"`
	Version   string `json:"version"`
}

type App struct {
	mu         sync.Mutex
	dataDir    string
	storePath  string
	nativePath string
	startedAt  time.Time
	config     Config
	tasks      map[string]*Task
	native     map[string]*NativeAccount
	logins     map[string]*NativeLogin
	qrLogins   map[string]*NativeQRLogin
	running    map[string]chan struct{}
	// healthRunning 记录当前有自动健康检查在跑的账号（R4.1 去重）。
	healthRunning map[string]bool
	// accessRecheck 记录各账号上次「访问时未就绪补检」时间（冷却去重，见 healthauto.go）。
	accessRecheck map[string]time.Time
	// taskSpawns 记录每任务的最近调度时间与连续爆发次数（R4.26 调度限流）：
	// 无论上游是谁在反复复活任务（ensure 轮询复活 error 任务、用户狂点、幻影清理
	// 竞态），调度层对同一任务的 spawn 频率做硬限制，掐断「复活→失败→再复活」
	// 紧循环（2.6.3 实测：同一任务同一秒刷出几十条 stalled/failed 日志）。
	taskSpawns map[string]*spawnStat
	// taskLogs 记录每任务各类日志的最近输出时间（R4.26 日志限频），防止异常
	// 场景下同一条错误刷爆日志。
	taskLogs map[string]*taskLogState
	// mediaConns 是账号级常驻 MTProto 连接池（R4.29）：每账号一条连接、
	// 每媒体 DC 只 exportAuth 一次，根治反复 export 触发的 FLOOD_WAIT。
	// 独立锁 mediaMu，与 a.mu 无环（见 mediapool.go 锁纪律）。
	mediaMu    sync.Mutex
	mediaConns map[string]*mediaConn
	// mediaProbes 是各账号最近一次媒体 DC 分级探测快照（R4.29），随 /api/state
	// 进诊断页；读写在 a.mu 下进行（探测的网络 IO 在锁外完成）。
	mediaProbes map[string]mediaProbeSnapshot
	// lastAutoSpawn 记录上次调度后台缓存（auto）任务的时间（R4.29 错峰），
	// 与手动下载之间保持 autoSpawnMinInterval 的最小间隔。
	lastAutoSpawn time.Time
	client        *http.Client
	// proxy 是当前生效的网络代理（MTProto dialer + 媒体 transport 共用），
	// 用独立锁保护，避免与 App.mu 相互等待。详见 proxy.go。
	proxy *proxyRuntime
}

func main() {
	dataDir := env("FEIGRAM_DOWNLOADER_DATA", filepath.Join(env("DATA_DIR", "data"), "downloader"))
	port := env("FEIGRAM_DOWNLOADER_PORT", "3090")
	// 先建代理运行时，让 App 与媒体 transport 共用同一个实例。
	proxy := newProxyRuntime()
	app := &App{
		dataDir:    dataDir,
		storePath:  filepath.Join(dataDir, "tasks.json"),
		nativePath: filepath.Join(dataDir, "native-sessions.json"),
		startedAt:  time.Now(),
		proxy:      proxy,
		config: Config{
			Enabled:      true,
			Concurrency:  1,
			RateLimitBps: 0,
			Mode:         "conservative",
			PartSize:     defaultPartSize,
			Backend:      "go-sidecar",
			// M3.1：默认走 Go 原生 MTProto；http-bridge 仅作为显式降级开关保留。
			Transport: "native-mtproto",
			// 应用内代理留空，由 proxyRuntime 决定是否回落到环境变量。
			ProxyURL:  "",
			UpdatedAt: now(),
		},
		tasks:       map[string]*Task{},
		native:      map[string]*NativeAccount{},
		logins:      map[string]*NativeLogin{},
		qrLogins:    map[string]*NativeQRLogin{},
		running:     map[string]chan struct{}{},
		taskSpawns:  map[string]*spawnStat{},
		taskLogs:    map[string]*taskLogState{},
		mediaConns:  map[string]*mediaConn{},
		mediaProbes: map[string]mediaProbeSnapshot{},
		client: &http.Client{
			Timeout:   0,
			Transport: newMediaTransport(proxy),
		},
	}
	if err := app.load(); err != nil {
		log.Printf("load store: %v", err)
	}
	// R4.30：peer 索引落盘（路径绑定 + 历史恢复）。必须在任何会话/下载活动之前，
	// 让 file_reference 自愈在重启后直接命中上次解析过的频道。
	initNativePeerIndexStore(dataDir)
	if _, reason := app.proxy.apply(app.config.ProxyURL); reason != "" {
		log.Printf("网络代理配置无效：%s", reason)
	}
	log.Printf("%s", app.proxy.describeProxyConfig())
	go app.pump()
	go app.healthLoop()
	// R4.21：启动即对未就绪账号补检，新版诊断信息不再等冷却。
	go app.bootstrapHealthChecks()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", app.handleHealth)
	mux.HandleFunc("/api/state", app.handleState)
	mux.HandleFunc("/api/config", app.handleConfig)
	mux.HandleFunc("/api/native/accounts", app.handleNativeAccounts)
	mux.HandleFunc("/api/native/accounts/", app.handleNativeAccount)
	mux.HandleFunc("/api/auth/", app.handleAuth)
	// M4.2：Go Telegram Core 直接提供聊天/文件夹/消息/头像/peer 解析能力。
	// 精确路径 /api/accounts/migrate 由 mux 优先匹配，不会落入本子树。
	mux.HandleFunc("/api/accounts/", app.handleAccountAPI)
	mux.HandleFunc("/api/accounts/migrate", app.handleMigrate)
	mux.HandleFunc("/api/tasks", app.handleTasks)
	mux.HandleFunc("/api/tasks/", app.handleTask)

	server := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           withJSON(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("Feigram Downloader %s listening on %s", version, server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (a *App) load() error {
	if err := os.MkdirAll(a.dataDir, 0o755); err != nil {
		return err
	}
	raw, err := os.ReadFile(a.storePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return a.saveLocked()
		}
		return err
	}
	var store Store
	if err := json.Unmarshal(raw, &store); err != nil {
		return err
	}
	if store.Config.Concurrency > 0 {
		a.config = sanitizeConfig(store.Config)
	}
	for i := range store.Tasks {
		task := store.Tasks[i]
		if task.ID == "" {
			task.ID = taskID(task.UserID, task.AccountID, task.PeerID, task.MessageID)
		}
		if task.Status == "downloading" || task.Status == "running" {
			task.Status = "queued"
			task.SpeedBps = 0
			task.Error = "Go 下载服务重启，已等待续传"
		}
		// R4.22：此前把「session 未就绪」的失败任务强行改写成 http-bridge 续传，
		// 而那个「回退源」只会绕回同一个坏账号。现在改回默认传输、等账号恢复：
		// 健康检查通过后由 pump 自动拉起，用户不需要理解「传输方式」这层概念。
		if task.Status == "error" && strings.Contains(task.Error, "未就绪") {
			task.Status = "queued"
			task.Transport = ""
			task.RetryAfter = 0
			task.Error = "账号未就绪，等待健康检查通过后自动续传"
		}
		// R4.23：2.5.x/2.6.0 落盘的 http-bridge 任务，其源全部是 Go blob 自环源
		//（M4.1 后 Node 已无外部媒体桥）。落盘时直接归一化回默认传输，
		// 不再依赖运行时 taskTransport 的兜底判断。
		if task.Transport == "http-bridge" && isGoBlobSourceURL(task.SourceURL) {
			task.Transport = ""
		}
		// R4.27：2.6.4 的刷新链路断裂（channel accessHash 缺失 + Node 元数据桥自环）
		// 把一批 file_reference 过期的任务误判成终态失败。升级后自动复活为排队，
		// 由修复后的刷新链路（peer 索引自愈 + FLOOD_WAIT 精确等待）接管续传。
		if task.Status == "error" && strings.Contains(task.Error, "metadata refresh url is empty") {
			task.Status = "queued"
			task.RetryAfter = 0
			task.Error = "2.6.5 修复刷新链路后自动复活，等待续传"
		}
		if task.Status != "downloading" && task.Status != "running" {
			task.SpeedBps = 0
		}
		a.tasks[task.ID] = &task
	}
	if err := a.loadNativeLocked(); err != nil {
		log.Printf("load native sessions: %v", err)
	}
	return nil
}

func (a *App) loadNativeLocked() error {
	raw, err := os.ReadFile(a.nativePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return a.saveNativeLocked()
		}
		return err
	}
	var store NativeStore
	if err := json.Unmarshal(raw, &store); err != nil {
		return err
	}
	for i := range store.Accounts {
		account := store.Accounts[i]
		if account.UserID == "" || account.AccountID == "" {
			continue
		}
		account.Status = normalizeNativeStatus(account.Status, account.Ready)
		a.native[nativeAccountKey(account.UserID, account.AccountID)] = &account
	}
	return nil
}

func (a *App) saveLocked() error {
	tasks := a.listTasksLocked()
	store := Store{
		Config: a.config,
		Tasks:  tasks,
		Meta: Meta{
			StartedAt: a.startedAt.Format(time.RFC3339),
			PID:       os.Getpid(),
			Version:   version,
		},
	}
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.storePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.storePath)
}

func (a *App) saveNativeLocked() error {
	accounts := make([]NativeAccount, 0, len(a.native))
	for _, account := range a.native {
		copy := *account
		accounts = append(accounts, copy)
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].UserID != accounts[j].UserID {
			return accounts[i].UserID < accounts[j].UserID
		}
		return accounts[i].AccountID < accounts[j].AccountID
	})
	raw, err := json.MarshalIndent(NativeStore{Accounts: accounts}, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.nativePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.nativePath)
}

func (a *App) pump() {
	for {
		started := a.pumpOnce()
		if !started {
			time.Sleep(800 * time.Millisecond)
		}
	}
}

// --- R4.26：调度限流 + 日志限频 ---
//
// 2.6.3 实测暴露的结构性缺口：running 守卫只能防「同任务并发重复」，
// 防不了「快速失败 → 外部复活（ensure 轮询把 error 任务改回 queued 并清零退避）→
// 再调度」的紧循环，表现为同一任务在同一秒内刷出几十条 stalled/failed 日志。
// 这里在调度层加两道与上游无关的硬闸：
//  1. spawn 限流：同任务两次调度至少间隔 taskSpawnMinInterval；
//     间隔内重复调度累计爆发次数，超过阈值进入指数冷却；
//  2. 日志限频：同任务同一条错误在窗口内只打一条，其余计数后汇总。

const (
	taskSpawnMinInterval  = 10 * time.Second
	taskSpawnBurstLimit   = 3
	taskSpawnCooldownStep = 60 * time.Second
	taskSpawnCooldownMax  = 5 * time.Minute
	taskLogDedupWindow    = 60 * time.Second
)

type spawnStat struct {
	last          time.Time
	bursts        int
	cooldownUntil time.Time
}

type taskLogState struct {
	last       map[string]time.Time
	suppressed map[string]int
}

// spawnAllowedLocked 判定任务此刻是否允许被调度启动（调用方须持 a.mu）。
// 返回 (false, cooldown>0) 表示处于冷却期；爆发次数触顶时内部会设置冷却。
func (a *App) spawnAllowedLocked(id string) (bool, time.Duration) {
	nowTs := time.Now()
	stat := a.taskSpawns[id]
	if stat == nil {
		return true, 0
	}
	if nowTs.Before(stat.cooldownUntil) {
		return false, stat.cooldownUntil.Sub(nowTs)
	}
	if nowTs.Sub(stat.last) < taskSpawnMinInterval {
		stat.bursts++
		if stat.bursts >= taskSpawnBurstLimit {
			shift := minInt(stat.bursts-taskSpawnBurstLimit+1, 4)
			cooldown := taskSpawnCooldownStep * (1 << shift)
			if cooldown > taskSpawnCooldownMax {
				cooldown = taskSpawnCooldownMax
			}
			stat.cooldownUntil = nowTs.Add(cooldown)
			stat.bursts = 0
			log.Printf("task %s: 检测到调度紧循环（%s 内第 %d 次重复调度），进入 %s 调度冷却（R4.26）", id, taskSpawnMinInterval, taskSpawnBurstLimit, cooldown)
			return false, cooldown
		}
		return true, 0
	}
	stat.bursts = 0
	return true, 0
}

// markSpawnLocked 记录一次调度启动（调用方须持 a.mu）。
func (a *App) markSpawnLocked(id string) {
	if a.taskSpawns == nil {
		a.taskSpawns = map[string]*spawnStat{}
	}
	stat := a.taskSpawns[id]
	if stat == nil {
		stat = &spawnStat{}
		a.taskSpawns[id] = stat
	}
	stat.last = time.Now()
}

// taskEventLog 同任务同一条错误在 taskLogDedupWindow 内只打一条日志，
// 其余计数；窗口后再次出现时先补一条汇总。必须在未持有 a.mu 的上下文调用。
func (a *App) taskEventLog(taskID, message string) {
	a.mu.Lock()
	if a.taskLogs == nil {
		a.taskLogs = map[string]*taskLogState{}
	}
	state := a.taskLogs[taskID]
	if state == nil {
		state = &taskLogState{last: map[string]time.Time{}, suppressed: map[string]int{}}
		a.taskLogs[taskID] = state
	}
	nowTs := time.Now()
	if last, ok := state.last[message]; ok && nowTs.Sub(last) < taskLogDedupWindow {
		state.suppressed[message]++
		a.mu.Unlock()
		return
	}
	state.last[message] = nowTs
	suppressed := state.suppressed[message]
	state.suppressed[message] = 0
	a.mu.Unlock()
	if suppressed > 0 {
		log.Printf("task %s: %s（此前 %s 内同类日志已抑制 %d 条）", taskID, message, taskLogDedupWindow, suppressed)
		return
	}
	log.Printf("task %s: %s", taskID, message)
}

// taskCanStartLocked 判定任务此刻是否具备启动条件（调用方须持 a.mu）。
// 语义比 download() 的运行时处理**更严格一层**（提前止损，而非起了再失败）：
//   - http-bridge：必须有可拉取的 SourceURL（显式降级开关）；
//   - native-mtproto：账号必须 eligible —— R4.22 起这是一票否决，
//     不再用「带 SourceURL 就放行、运行时回退 HTTP」的方式放行坏账号任务（回退已移除）。
//
// 效果：坏账号任务留在 queued 并显示可读等待原因，不占并发坑；账号恢复后由
// 下一轮 pump 自然拉起，无需用户手动重试。
func (a *App) taskCanStartLocked(task *Task, transport string) bool {
	switch transport {
	case "http-bridge":
		return task.SourceURL != ""
	case "native-mtproto":
		account, ok := a.native[nativeAccountKey(task.UserID, task.AccountID)]
		return ok && account != nil && nativeAccountEligible(*account)
	default:
		return false
	}
}

// taskWaitReasonLocked 返回任务当前无法启动的、面向用户的等待原因（调用方持 a.mu）。
// 返回空串表示原因未知（不覆盖已有错误信息）。
func (a *App) taskWaitReasonLocked(task *Task, transport string) string {
	switch transport {
	case "http-bridge":
		if task.SourceURL == "" {
			return "媒体源缺失，等待重新拉取下载地址"
		}
	case "native-mtproto":
		account, ok := a.native[nativeAccountKey(task.UserID, task.AccountID)]
		if !ok || account == nil {
			return "Telegram 账号记录不存在，请在账号管理中重新登录"
		}
		if !nativeAccountEligible(*account) {
			// R4.22：把账号自身的真实原因（健康检查诊断结论）一并带出，
			// 用户不必再去翻服务端日志才知道卡在哪。
			reason := strings.TrimSpace(account.Error)
			if reason == "" {
				reason = "等待健康检查通过"
			}
			return "Telegram 账号尚未就绪（" + reason + "），下载将在账号恢复后自动继续"
		}
	}
	return ""
}

func (a *App) pumpOnce() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.config.Enabled {
		return false
	}
	limit := a.config.Concurrency
	if a.config.Mode != "fast" {
		limit = 1
	}
	if limit < 1 {
		limit = 1
	}
	started := false
	saveNeeded := false
	// R4.25：清理「幻影 running 占坑」。running 表与任务状态是两份真相：
	// 若任务状态已被外部改回 queued/error/cancelled（用户点「开始」或重新入队），
	// 而旧 goroutine 因媒体路径挂死迟迟不退出，坑位就会永久占用——conservative
	// 并发为 1 时整条队列被一个僵尸条目冻死，且全程零日志零提示（2.6.2 实测症状）。
	for id, cancel := range a.running {
		stored := a.tasks[id]
		if stored != nil && (stored.Status == "downloading" || stored.Status == "running") {
			continue
		}
		status := "<已删除>"
		if stored != nil {
			status = stored.Status
		}
		delete(a.running, id)
		close(cancel)
		log.Printf("task %s: 清理幻影 running 占坑（当前状态=%s，旧 goroutine 将被取消）", id, status)
	}
	nowUnix := time.Now().Unix()
	// R4.29：后台缓存让路。手动任务（用户点下载/续传）优先占满并发；
	// 后台缓存（source=auto / autoCache）只在没有任何手动任务等待时启动，
	// 且两次 auto 调度至少间隔 autoSpawnMinInterval——避免 46 个缓存任务与
	// 手动下载抢同一账号的 exportAuth 配额触发 FLOOD_WAIT（2.6.4 实测）。
	tasksSnapshot := a.listTasksLocked()
	isAutoCache := func(t Task) bool { return t.AutoCache || t.Source == "auto" }
	manualWaiting := false
	autoRunning := 0
	for _, t := range tasksSnapshot {
		if t.Status == "queued" && !isAutoCache(t) && t.FilePath != "" && t.RetryAfter <= nowUnix {
			manualWaiting = true
		}
	}
	for id := range a.running {
		if stored := a.tasks[id]; stored != nil && isAutoCache(*stored) {
			autoRunning++
		}
	}
	for _, task := range tasksSnapshot {
		// R4.30：并发坑满不再 break——break 会跳过后续任务的「等待原因可见化」
		//（R4.22/R4.29 的可见化契约被随机顺序偶发打破：listTasksLocked 按
		// CreatedAt+稳定序排，同一秒创建的任务顺序取决于 map 遍历序）。改为
		// 继续遍历：让路/冷却/缺路径等原因照常写入，仅在真正 spawn 前拦截。
		if task.Status != "queued" {
			continue
		}
		if task.FilePath == "" {
			// R4.25：缺失落盘路径不再是静默跳过——补写等待原因，与 R4.11 的
			// 「等待原因可见化」一致。此前这里零日志零提示，是排障盲区之一。
			if task.Error != errMissingFilePathReason {
				if stored := a.tasks[task.ID]; stored != nil {
					stored.Error = errMissingFilePathReason
					stored.UpdatedAt = now()
					saveNeeded = true
				}
			}
			continue
		}
		transport := a.taskTransport(task)
		// R4.7：跨账号高速（fast）批量缓存时，绑定到坏账号的任务若照常起 goroutine，
		// 会占满并发坑、空跑到失败，把整批任务拖死。调度前统一校验，不具备可启动条件
		// （HTTP 无源 / native 账号不健康）的任务先不调度，账号恢复后自然可再跑。
		if !a.taskCanStartLocked(&task, transport) {
			// R4.11：等待原因可见化——任务留在队列但把原因写到 Error 字段，
			// 用户在下载中心能看见「为什么一直没动」，而不是静默卡住。
			// 仅在原因变化时写库，避免调度循环每 800ms 刷一次盘。
			if reason := a.taskWaitReasonLocked(&task, transport); reason != "" && task.Error != reason {
				if stored := a.tasks[task.ID]; stored != nil {
					stored.Error = reason
					stored.UpdatedAt = now()
					saveNeeded = true
				}
			}
			continue
		}
		if task.RetryAfter > nowUnix {
			continue
		}
		// R4.29：auto（后台缓存）让路判定——手动等待时不启动 auto；auto 同时最多
		// 一个在跑；两次 auto 调度至少间隔 autoSpawnMinInterval。被让路的任务留
		// 在 queued，把原因可见化（仅在原因变化时写盘，避免 800ms 调度循环刷库）。
		if isAutoCache(task) {
			if manualWaiting {
				if stored := a.tasks[task.ID]; stored != nil {
					reason := "后台缓存等待手动下载完成（手动优先，R4.29）"
					if stored.Error != reason {
						stored.Error = reason
						stored.UpdatedAt = now()
						saveNeeded = true
					}
				}
				continue
			}
			if autoRunning >= 1 {
				continue
			}
			if time.Since(a.lastAutoSpawn) < autoSpawnMinInterval {
				continue
			}
		}
		if _, ok := a.running[task.ID]; ok {
			continue
		}
		// R4.26：任务级调度限流。running 守卫只防「并发重复」，
		// 防不了「快速失败 → 被外部复活 → 再调度」的紧循环——那是
		// 2.6.3 实测日志风暴（同一任务同秒几十条 stalled/failed）的直接来源。
		if allowed, cooldown := a.spawnAllowedLocked(task.ID); !allowed {
			continue
		} else if cooldown > 0 {
			// 冷却期任务留在 queued，把原因写给用户看。
			if stored := a.tasks[task.ID]; stored != nil {
				reason := fmt.Sprintf("调度冷却中（%s 后自动重试，R4.26 防调度紧循环）", formatDuration(cooldown))
				if stored.Error != reason {
					stored.Error = reason
					stored.UpdatedAt = now()
					saveNeeded = true
				}
			}
			continue
		}
		// R4.30：并发坑满只拦 spawn，不拦等待原因写入（见循环头注释）。
		if len(a.running) >= limit {
			continue
		}
		cancel := make(chan struct{})
		a.running[task.ID] = cancel
		a.markSpawnLocked(task.ID)
		// R4.26 根因修复：task 是 listTasksLocked() 返回的**值拷贝**，
		// 此前 `task.Status = "downloading"` 只改了副本，存储任务永远停在
		// queued——下一轮 pumpOnce 的幻影清理立即把刚 spawn 的坑当僵尸杀掉
		// 再重新 spawn，形成 spawn/误杀循环：2.6.2「永远排队、运行 0/1」、
		// 2.6.3「同秒几十条 stalled/failed 日志风暴」的共同根因。
		// 状态必须写到 a.tasks 里的存储任务上。
		if stored := a.tasks[task.ID]; stored != nil {
			stored.Status = "downloading"
			stored.Error = ""
			stored.UpdatedAt = now()
		}
		started = true
		// R4.25：任务启动必须留日志。此前启动无日志、失败才有日志，
		// 「任务到底有没有被调度」在用户日志里无从判断（2.6.2 排障盲区）。
		log.Printf("task %s start: transport=%s offset=%d/%d file=%s", task.ID, transport, task.Downloaded, task.Size, task.FilePath)
		if isAutoCache(task) {
			a.lastAutoSpawn = time.Now()
			autoRunning++
		}
		go a.runTask(task.ID, cancel)
	}
	if started || saveNeeded {
		_ = a.saveLocked()
	}
	return started
}

func (a *App) runTask(id string, cancel <-chan struct{}) {
	defer func() {
		a.mu.Lock()
		// R4.26：goroutine 退出审计。坑位在本 goroutine 存活期间就被移除
		//（幻影清理/强制重启/删除任务）说明调度层与执行层出现过状态竞争，
		// 留一条日志供排障定位（正常退出时条目仍在，不会有这条日志）。
		if _, alive := a.running[id]; !alive {
			log.Printf("task %s: goroutine 退出时发现调度坑位已被回收（幻影清理或强制重启）", id)
		}
		delete(a.running, id)
		_ = a.saveLocked()
		a.mu.Unlock()
	}()

	for {
		task := a.taskSnapshot(id)
		if task == nil {
			return
		}
		// R4.25：本 goroutine 是否仍是任务的「现任」执行者。queue 强制重启、
		// 幻影坑清理都会在保留旧 goroutine 的情况下重置任务状态；旧 goroutine
		// 醒来后不得覆盖新状态（否则会把刚排队的任务改回 cancelled/error，
		// 用户看到的仍是「点开始没反应」）。
		stillCurrent := func(t *Task) bool {
			return t.Status == "downloading" || t.Status == "running"
		}
		if err := a.download(task, cancel); err != nil {
			if errors.Is(err, errCancelled) {
				a.updateTask(id, func(t *Task) {
					if !stillCurrent(t) {
						return
					}
					t.Status = "cancelled"
					t.SpeedBps = 0
					t.Error = ""
					t.RetryAfter = 0
					t.UpdatedAt = now()
				})
				return
			}
			if transientSourceError(err) {
				nextCount := task.RetryCount + 1
				if nextCount > maxTransientRetries {
					a.updateTask(id, func(t *Task) {
						if !stillCurrent(t) {
							return
						}
						t.Status = "error"
						t.SpeedBps = 0
						t.RetryCount = nextCount
						t.RetryAfter = 0
						t.Error = fmt.Sprintf("媒体源长时间不可用，已自动重试 %d 次后停止；请稍后手动重试", nextCount-1)
						t.UpdatedAt = now()
					})
					log.Printf("task %s failed after %d transient retries: %v", id, nextCount-1, err)
					return
				}
				delay := retryDelay(nextCount)
				// R4.27：FLOOD_WAIT 按 Telegram 给出的秒数精确等待，不再套用
				// 指数退避——限流窗口 1400+ 秒时，5 分钟封顶的退避会让任务
				// 反复撞墙并加深限流。
				reason := "媒体源暂不可用"
				if wait := floodWaitFromError(err); wait > 0 {
					delay = time.Duration(wait) * time.Second
					if delay > floodWaitBackoffCap {
						delay = floodWaitBackoffCap
					}
					reason = "Telegram 限流（FLOOD_WAIT）"
				}
				a.updateTask(id, func(t *Task) {
					if !stillCurrent(t) {
						return
					}
					t.Status = "queued"
					t.SpeedBps = 0
					t.RetryCount = nextCount
					t.RetryAfter = time.Now().Add(delay).Unix()
					t.Error = fmt.Sprintf("%s，%s 后自动续传：%s", reason, formatDuration(delay), compactError(err))
					t.UpdatedAt = now()
				})
				a.taskEventLog(id, fmt.Sprintf("transient failure, retry in %s: %v", delay, err))
				return
			}
			a.updateTask(id, func(t *Task) {
				if !stillCurrent(t) {
					return
				}
				t.Status = "error"
				t.SpeedBps = 0
				t.Error = err.Error()
				t.RetryAfter = 0
				t.UpdatedAt = now()
			})
			// R4.11：仅 native 传输的真实失败计入账号连续失败；HTTP 回退的失败是媒体源问题，
			// 不该给账号健康度记黑账。
			if task.Transport == "native-mtproto" {
				a.markNativeAccountResult(task.UserID, task.AccountID, false)
			}
			a.taskEventLog(id, fmt.Sprintf("failed: %v", err))
			return
		}
		a.updateTask(id, func(t *Task) {
			if !stillCurrent(t) {
				return
			}
			t.Status = "completed"
			if stat, err := os.Stat(t.FilePath); err == nil {
				t.Downloaded = stat.Size()
				if t.Size <= 0 || stat.Size() > t.Size {
					t.Size = stat.Size()
				}
			}
			t.SpeedBps = 0
			t.Error = ""
			t.RetryCount = 0
			t.RetryAfter = 0
			t.UpdatedAt = now()
		})
		if task.Transport == "native-mtproto" {
			a.markNativeAccountResult(task.UserID, task.AccountID, true)
		}
		return
	}
}

var errCancelled = errors.New("cancelled")

// errAccountNotReady 表示「账号此刻不可用」。它不是终态错误：调度层会等账号恢复后
// 自动续传（见 transientSourceError），因此必须带上账号自身的真实原因。
var errAccountNotReady = errors.New("Telegram 账号尚未就绪")

func (a *App) download(task *Task, cancel <-chan struct{}) error {
	switch a.taskTransport(*task) {
	case "native-mtproto":
		account, err := a.nativeAccountSnapshot(task.UserID, task.AccountID)
		if err != nil || !nativeAccountEligible(account) {
			// R4.22：账号不可用就直说，不再回退 HTTP 桥。
			// 缘由：M4.1 已删除 Node 侧内部媒体桥，下载任务的 SourceURL 现在指向
			// Go 自己的 blob 端点（见 server/src/telegramService.js 的 goBlobSourceUrl），
			// 所谓「回退」只是绕回同一个坏账号，把真实原因二次包装成
			// `source returned 404: {"error":"Go 原生账号 ... 尚未就绪..."}` 这种三层转述，
			// 正是用户反馈「为什么这么复杂」的现场（2.5.6 实测日志）。
			return accountNotReadyError(task, account, err)
		}
		return a.downloadNativeMTProto(task, cancel)
	default:
		return a.downloadHTTPBridge(task, cancel)
	}
}

// accountNotReadyError 构造「账号未就绪」错误：直出账号记录里的真实原因
// （通常是健康检查的诊断结论，如「分级探测：TCP 拨号即失败…」），并带上等待语义。
func accountNotReadyError(task *Task, account NativeAccount, lookupErr error) error {
	if lookupErr != nil {
		return fmt.Errorf("%w（%s/%s）：%s", errAccountNotReady, task.UserID, task.AccountID, compactError(lookupErr))
	}
	reason := strings.TrimSpace(account.Error)
	if reason == "" {
		reason = "等待健康检查通过"
	}
	return fmt.Errorf("%w（%s），下载将在账号恢复后自动续传：%s", errAccountNotReady, coalesce(account.Status, "unknown"), reason)
}

// storageErrorHint 把下载目录权限错误翻译成可操作的提示（R4.19，2.5.3 实测反馈）：
// 飞牛上应用以独立用户运行，下载目录被改到用户自选位置（如 /vol2/1000/movie/feigrampub）
// 时常因目录未对应用用户开放写权限而 EACCES——裸报 "permission denied" 用户无从下手。
func storageErrorHint(err error, dir string) error {
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return fmt.Errorf(
		"%w —— 下载目录权限不足：应用运行用户对 %s 没有写权限。请在飞牛文件管理中给该目录开放写权限（或把目录归属改为应用运行用户），或在「设置 → 下载目录」改用应用可写目录（默认 data/downloads）",
		err, dir,
	)
}

func (a *App) downloadHTTPBridge(task *Task, cancel <-chan struct{}) error {
	if err := os.MkdirAll(filepath.Dir(task.FilePath), 0o755); err != nil {
		return storageErrorHint(err, filepath.Dir(task.FilePath))
	}
	if task.SourceURL == "" {
		return errors.New("HTTP 桥接媒体源为空，无法开始下载")
	}
	if task.PartPath == "" {
		task.PartPath = task.FilePath + ".part"
	}
	if stat, err := os.Stat(task.FilePath); err == nil && complete(stat.Size(), task.Size) {
		return nil
	}
	downloaded := int64(0)
	if stat, err := os.Stat(task.PartPath); err == nil {
		downloaded = stat.Size()
	}
	if task.Size > 0 && downloaded > task.Size {
		_ = os.Remove(task.PartPath)
		downloaded = 0
	}

	req, err := http.NewRequest(http.MethodGet, task.SourceURL, nil)
	if err != nil {
		return err
	}
	if downloaded > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", downloaded))
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && downloaded > 0 {
		_ = os.Remove(task.PartPath)
		downloaded = 0
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// R4.23：源是我们自己的 blob 端点时，404 body 里带「尚未就绪」说明账号
		// 此刻不可用——这不是媒体源故障，必须归类为瞬态（errAccountNotReady），
		// 账号恢复后自动续传；否则任务被记成终态错误，用户只能手动重试
		//（2.6.0 实测：14:27:54 任务终态失败，14:27:56 账号已恢复却无人续传）。
		if resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), "尚未就绪") {
			account, accountErr := a.nativeAccountSnapshot(task.UserID, task.AccountID)
			return accountNotReadyError(task, account, accountErr)
		}
		return fmt.Errorf("source returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if task.Size <= 0 && resp.ContentLength > 0 {
		task.Size = downloaded + resp.ContentLength
	}
	file, err := os.OpenFile(task.PartPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return storageErrorHint(err, filepath.Dir(task.FilePath))
	}
	defer file.Close()

	buffer := make([]byte, 128*1024)
	lastBytes := downloaded
	lastTick := time.Now()
	windowStart := time.Now()
	var windowBytes int64
	a.updateTask(task.ID, func(t *Task) {
		t.Downloaded = downloaded
		t.Size = max64(t.Size, task.Size)
		t.UpdatedAt = now()
	})
	for {
		select {
		case <-cancel:
			return errCancelled
		default:
		}
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, err := file.Write(buffer[:n]); err != nil {
				return err
			}
			downloaded += int64(n)
			windowBytes += int64(n)
			if err := a.throttle(windowBytes, windowStart, cancel); err != nil {
				return err
			}
			if a.config.RateLimitBps > 0 && time.Since(windowStart) >= time.Second {
				windowStart = time.Now()
				windowBytes = 0
			}
			if time.Since(lastTick) >= time.Second {
				elapsed := time.Since(lastTick).Seconds()
				speed := int64(float64(downloaded-lastBytes) / maxFloat(elapsed, 0.001))
				lastBytes = downloaded
				lastTick = time.Now()
				a.updateTask(task.ID, func(t *Task) {
					t.Downloaded = downloaded
					t.SpeedBps = speed
					t.Size = max64(t.Size, task.Size)
					t.UpdatedAt = now()
				})
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	stat, err := os.Stat(task.PartPath)
	if err != nil {
		return err
	}
	size := max64(task.Size, stat.Size())
	if !complete(stat.Size(), size) {
		return fmt.Errorf("file incomplete: %d / %d", stat.Size(), size)
	}
	if err := os.Rename(task.PartPath, task.FilePath); err != nil {
		return err
	}
	return nil
}

func (a *App) downloadNativeMTProto(task *Task, cancel <-chan struct{}) error {
	select {
	case <-cancel:
		return errCancelled
	default:
	}
	if task.PartPath == "" {
		task.PartPath = task.FilePath + ".part"
	}
	if task.NativeFile.FileID == "" || task.NativeFile.AccessHash == "" || task.NativeFile.FileReference == "" {
		return fmt.Errorf("Go 原生 MTProto 缺少 file location 元数据，无法调用 upload.getFile")
	}
	account, err := a.nativeAccountSnapshot(task.UserID, task.AccountID)
	if err != nil {
		return err
	}
	if !account.Ready || account.Session == "" {
		return fmt.Errorf("Go 原生 MTProto session 未就绪，请先在管理后台完成 Go 重新登录和健康检查")
	}
	apiHash, err := a.nativeAPIHash(account)
	if err != nil {
		return err
	}
	if account.APIID <= 0 || apiHash == "" {
		return fmt.Errorf("Go 原生 MTProto 缺少 API ID/Hash，请重新同步服务端设置")
	}
	if err := os.MkdirAll(filepath.Dir(task.FilePath), 0o755); err != nil {
		return storageErrorHint(err, filepath.Dir(task.FilePath))
	}
	if stat, err := os.Stat(task.FilePath); err == nil && complete(stat.Size(), task.Size) {
		return nil
	}
	downloaded := int64(0)
	if stat, err := os.Stat(task.PartPath); err == nil {
		downloaded = stat.Size()
	}
	if task.Size > 0 && downloaded > task.Size {
		_ = os.Remove(task.PartPath)
		downloaded = 0
	}
	file, err := os.OpenFile(task.PartPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return storageErrorHint(err, filepath.Dir(task.FilePath))
	}
	defer file.Close()

	fileID, err := strconv.ParseInt(task.NativeFile.FileID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid native file id: %w", err)
	}
	accessHash, err := strconv.ParseInt(task.NativeFile.AccessHash, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid native access hash: %w", err)
	}
	fileReference, err := base64.StdEncoding.DecodeString(task.NativeFile.FileReference)
	if err != nil {
		return fmt.Errorf("invalid native file reference: %w", err)
	}
	if task.NativeFile.DCID > 0 {
		account.Status = "healthy"
	}

	// R4.29：改用账号级常驻连接池（mediapool.go）。此前每任务 newTelegramClient
	// 新建客户端，媒体 DC 的 exportAuth 随任务反复发起，同一账号短时间几十次
	// export 后被 Telegram FLOOD_WAIT 1400+ 秒（2.6.4/2.6.5 实测）。常驻连接的
	// DC 池随连接保活——每个媒体 DC 只 exportAuth 一次，后续任务直接复用。
	primaryDC := a.nativePrimaryDC(task.UserID, task.AccountID)
	conn, err := a.acquireMediaConn(account, apiHash)
	if err != nil {
		return errors.New(a.withNetworkHint(err))
	}
	client := conn.client

	// R4.25：无进度看门狗。此前这里只有 WithCancel——媒体路径一旦挂死
	//（DC 路由异常/代理不放行媒体段/对端无响应），goroutine 永久悬挂：
	// 无日志、任务卡在传输态，running 坑被占死，conservative 单并发下整个
	// 队列静默冻结（2.6.2 实测「一直排队、点开始没反应」的根因）。
	// 现在任何 120 秒窗口内零字节就终止任务，转为可诊断的瞬态错误自动续传。
	// R4.28：两档阈值——本次尝试**一个字节都没收到**（progressSeen=false）时
	// 用 30s 首字节超时快速失败：媒体路径不通时（2.6.5 实测每次 0/171MB
	// 白等 120s），单并发下一任务就白白占死队列 2 分钟；收到过字节后仍用
	// 120s 窗口容忍正常的网络抖动/慢速分块。
	// R4.29：0 字节终止前先对该任务的媒体 DC 做一次分级探测，把「代理未放行
	// 媒体网段」vs「节点转发质量」的结论直接写进任务错误，终结盲猜。
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	lastProgress := time.Now()
	progressSeen := false
	go func() {
		watch := time.NewTicker(10 * time.Second)
		defer watch.Stop()
		for {
			select {
			case <-cancel:
				stop(errCancelled)
				return
			case <-ctx.Done():
				return
			case <-watch.C:
				threshold := nativeNoProgressTimeout
				reason := "媒体路径无进度"
				if !progressSeen {
					threshold = nativeFirstByteTimeout
					reason = "连接媒体服务器后始终无响应"
				}
				if time.Since(lastProgress) > threshold {
					// R4.26：stalled 日志走限频。看门狗本身每个 goroutine 只打一次，
					// 但「复活→再挂死」的循环会让这条日志反复出现，限频防刷屏。
					diagDC := task.NativeFile.DCID
					if diagDC <= 0 {
						diagDC = primaryDC
					}
					diag := a.diagnoseMediaDC(diagDC)
					a.taskEventLog(task.ID, fmt.Sprintf("stalled: no bytes for %s（%s，看门狗终止）%s", threshold, reason, diag))
					stop(fmt.Errorf("%w；%s", errDownloadStalled, diag))
					return
				}
			}
		}
	}()
	metadataAPI := client.API()
	fileAPI := metadataAPI
	fileDC := task.NativeFile.DCID
	var mediaInvoker telegram.CloseInvoker
	closeMedia := func() {
		if mediaInvoker != nil {
			if err := mediaInvoker.Close(); err != nil {
				log.Printf("task %s close media DC %d invoker: %v", task.ID, fileDC, err)
			}
			mediaInvoker = nil
		}
	}
	defer closeMedia()
	switchToMediaDC := func(dc int) error {
		if dc <= 0 {
			fileAPI = metadataAPI
			closeMedia()
			fileDC = 0
			return nil
		}
		if mediaInvoker != nil && fileDC == dc {
			return nil
		}
		closeMedia()
		// R4.18：目标 DC 就是账号主 DC 时不能走 MediaOnly 池——
		// gotd 会 exportAuthorization(dc) 而 Telegram 对「导出到自己」返回
		// DC_ID_INVALID；直接复用主连接。
		if dc == primaryDC {
			mediaInvoker = nil
			fileAPI = metadataAPI
			fileDC = dc
			log.Printf("task %s media DC %d is primary, using primary connection for native upload.getFile", task.ID, dc)
			return nil
		}
		invoker, err := client.MediaOnly(ctx, dc, 1)
		if err != nil {
			fileAPI = metadataAPI
			fileDC = 0
			return fmt.Errorf("connect Telegram media DC %d: %w", dc, err)
		}
		mediaInvoker = invoker
		fileAPI = tg.NewClient(invoker)
		fileDC = dc
		log.Printf("task %s using Telegram media DC %d for native upload.getFile", task.ID, dc)
		return nil
	}
	if fileDC > 0 {
		if err := switchToMediaDC(fileDC); err != nil {
			log.Printf("task %s media DC %d unavailable, fallback current DC: %v", task.ID, fileDC, err)
		}
	}
	download := func() error {
		lastBytes := downloaded
		lastTick := time.Now()
		windowStart := time.Now()
		var windowBytes int64
		a.updateTask(task.ID, func(t *Task) {
			t.Downloaded = downloaded
			t.Size = max64(t.Size, task.Size)
			t.UpdatedAt = now()
		})
		// M3.3：file_reference 刷新预算，避免引用持续过期时无限续传。
		fileRefRefreshes := 0
		for {
			select {
			case <-ctx.Done():
				// R4.25：取消必须区分「用户取消」与「看门狗判定挂死」——
				// 直接取 context cause，两者各自带正确语义向上传递。
				return context.Cause(ctx)
			default:
			}
			if task.Size > 0 && downloaded >= task.Size {
				break
			}
			limit := int(a.currentPartSize())
			if limit <= 0 {
				limit = defaultPartSize
			}
			if task.Size > 0 && downloaded+int64(limit) > task.Size {
				limit = int(task.Size - downloaded)
			}
			location := &tg.InputDocumentFileLocation{
				ID:            fileID,
				AccessHash:    accessHash,
				FileReference: fileReference,
			}
			resp, err := fileAPI.UploadGetFile(ctx, &tg.UploadGetFileRequest{
				Location: location,
				Offset:   downloaded,
				Limit:    limit,
			})
			if err != nil {
				if isFileReferenceError(err) {
					// M3.3：刷新次数封顶，避免引用持续过期造成的无限续传。
					fileRefRefreshes++
					if !allowFileReferenceRefresh(fileRefRefreshes) {
						return fmt.Errorf("file_reference 已连续刷新 %d 次仍无法取流，停止任务：%w", fileRefRefreshes-1, err)
					}
					refreshed, refreshErr := a.refreshNativeFileLocation(ctx, metadataAPI, task.ID, account)
					if refreshErr != nil {
						return fmt.Errorf("file_reference 失效：自动刷新消息元数据失败：%w", refreshErr)
					}
					fileID, err = strconv.ParseInt(refreshed.FileID, 10, 64)
					if err != nil {
						return fmt.Errorf("invalid refreshed native file id: %w", err)
					}
					accessHash, err = strconv.ParseInt(refreshed.AccessHash, 10, 64)
					if err != nil {
						return fmt.Errorf("invalid refreshed native access hash: %w", err)
					}
					fileReference, err = base64.StdEncoding.DecodeString(refreshed.FileReference)
					if err != nil {
						return fmt.Errorf("invalid refreshed native file reference: %w", err)
					}
					task.NativeFile = refreshed
					// M3.3 故障注入测试点：生产恒为 nil，仅单元测试置位以覆盖「刷新后仍失败」。
					if hook := nativeFileRefRefreshHook; hook != nil {
						if hookErr := hook(fileRefRefreshes); hookErr != nil {
							return hookErr
						}
					}
					if refreshed.DCID > 0 && refreshed.DCID != fileDC {
						if err := switchToMediaDC(refreshed.DCID); err != nil {
							return classifyNativeReadError(err)
						}
					}
					log.Printf("task %s refreshed FILE_REFERENCE via Go message refetch and resumed at %d", task.ID, downloaded)
					continue
				}
				if migrateDC := migrationDC(err); migrateDC > 0 {
					log.Printf("task %s got Telegram DC migration request to %d at offset %d: %v", task.ID, migrateDC, downloaded, err)
					if err := switchToMediaDC(migrateDC); err != nil {
						return classifyNativeReadError(err)
					}
					continue
				}
				return classifyNativeReadError(err)
			}
			chunk, ok := resp.(*tg.UploadFile)
			if !ok {
				return fmt.Errorf("Go 原生 MTProto 暂不支持 CDN redirect 响应：%T", resp)
			}
			if len(chunk.Bytes) == 0 {
				break
			}
			if _, err := file.Write(chunk.Bytes); err != nil {
				return err
			}
			downloaded += int64(len(chunk.Bytes))
			lastProgress = time.Now() // R4.25：喂狗——任何真实字节流动都重置无进度计时。
			progressSeen = true       // R4.28：本次尝试已收到字节，看门狗切到 120s 常规窗口。
			windowBytes += int64(len(chunk.Bytes))
			if err := a.throttle(windowBytes, windowStart, cancel); err != nil {
				return err
			}
			if a.config.RateLimitBps > 0 && time.Since(windowStart) >= time.Second {
				windowStart = time.Now()
				windowBytes = 0
			}
			if time.Since(lastTick) >= time.Second {
				elapsed := time.Since(lastTick).Seconds()
				speed := int64(float64(downloaded-lastBytes) / maxFloat(elapsed, 0.001))
				lastBytes = downloaded
				lastTick = time.Now()
				a.updateTask(task.ID, func(t *Task) {
					t.Downloaded = downloaded
					t.SpeedBps = speed
					t.Size = max64(t.Size, task.Size)
					t.UpdatedAt = now()
				})
			}
		}
		return nil
	}
	runErr := download()
	if runErr != nil {
		return runErr
	}
	if err := file.Close(); err != nil {
		return err
	}
	stat, err := os.Stat(task.PartPath)
	if err != nil {
		return err
	}
	size := max64(task.Size, stat.Size())
	if !complete(stat.Size(), size) {
		if stat.Size() == 0 {
			// R4.26：0 字节返回按瞬态处理。实测（2.6.3）中它与「看门狗 stalled」
			// 成对出现：链路挂死期间 upload.GetFiles 返回空体 → Run 正常返回 →
			// 0/171931369 → 终态失败 → 被外部复活 → 再挂死，形成紧循环。
			// 零字节说明一个有效分块都没拿到，是链路/账号层故障而非文件损坏，
			// 应走退避续传；下载过一部分后的不完整仍是真异常，维持终态。
			return fmt.Errorf("媒体源返回空响应（已取 0 / %d 字节）：%w", size, errEmptyMediaResponse)
		}
		return fmt.Errorf("file incomplete: %d / %d", stat.Size(), size)
	}
	return os.Rename(task.PartPath, task.FilePath)
}

func (a *App) refreshNativeFileLocation(ctx context.Context, api *tg.Client, taskID string, account NativeAccount) (NativeFileLocation, error) {
	task := a.taskSnapshot(taskID)
	if task == nil {
		return NativeFileLocation{}, errors.New("task not found")
	}
	// R4.27：删除「回退 Node 元数据桥」分支。M4.1 后 Node 自身已无 MTProto 客户端，
	// 它的元数据接口内部仍然是调 Go 的原生 API——Go→Node→Go 纯自环（R4.22 同类问题），
	// 只会把真实失败原因包装成「metadata refresh url is empty」。现在直接透传
	// Go 侧刷新的真实错误（含自愈后的结论），配合瞬态表自动续传。
	return a.refreshNativeFileLocationFromTelegram(ctx, api, *task, account)
}

func (a *App) refreshNativeFileLocationFromTelegram(ctx context.Context, api *tg.Client, task Task, account NativeAccount) (NativeFileLocation, error) {
	if api == nil {
		return NativeFileLocation{}, errors.New("native api is nil")
	}
	messageID := int(task.MessageID)
	if messageID <= 0 {
		return NativeFileLocation{}, errors.New("native task missing message id")
	}
	peer := task.NativePeer
	// R4.27：channel 的 accessHash 只能从 dialogs/peer 索引获得。旧任务或
	// 「消息发送者」贫信息缓存会把它丢成空串——此前刷新直接失败，最终以
	// 「metadata refresh url is empty」终态收场。现在先经 resolveNativePeer
	// （本地 peer 索引 + 必要时回拉一次会话列表）自愈补全，再刷新消息元数据。
	// R4.30：自愈超时 45s→90s——未命中首批会话时会深翻分页（最多 12 轮 RPC），
	// 代理链路下 45s 可能不够走完全程。
	if strings.TrimSpace(peer.Type) == "" ||
		(strings.EqualFold(strings.TrimSpace(peer.Type), "channel") && strings.TrimSpace(peer.AccessHash) == "") {
		healCtx, healCancel := context.WithTimeout(ctx, 90*time.Second)
		healed, healErr := a.resolveNativePeer(healCtx, api, account, task.PeerID)
		healCancel()
		if healErr != nil {
			return NativeFileLocation{}, fmt.Errorf("native peer 元数据缺失（type=%q id=%q accessHash=%q）且自动解析失败：%w", peer.Type, peer.ID, peer.AccessHash, healErr)
		}
		peer = NativePeerLocation{Type: healed.Type, ID: healed.ID, AccessHash: healed.AccessHash}
		a.updateTask(task.ID, func(t *Task) {
			t.NativePeer = peer
			t.UpdatedAt = now()
		})
		log.Printf("task %s healed native peer metadata via peer index: type=%s id=%s", task.ID, peer.Type, peer.ID)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result tg.MessagesMessagesClass
	var err error
	peerType := strings.ToLower(strings.TrimSpace(peer.Type))
	switch peerType {
	case "channel":
		channelID, parseErr := strconv.ParseInt(peer.ID, 10, 64)
		if parseErr != nil || channelID == 0 {
			return NativeFileLocation{}, fmt.Errorf("invalid native channel id: %w", parseErr)
		}
		accessHash, parseErr := strconv.ParseInt(peer.AccessHash, 10, 64)
		if parseErr != nil {
			return NativeFileLocation{}, fmt.Errorf("invalid native channel access hash: %w", parseErr)
		}
		result, err = api.ChannelsGetMessages(reqCtx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: channelID, AccessHash: accessHash},
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
		})
	case "user", "chat":
		result, err = api.MessagesGetMessages(reqCtx, []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}})
	default:
		return NativeFileLocation{}, fmt.Errorf("unsupported native peer type %q", peer.Type)
	}
	if err != nil {
		return NativeFileLocation{}, classifyNativeReadError(err)
	}
	doc, err := nativeDocumentFromMessages(result, messageID)
	if err != nil && (peerType == "user" || peerType == "chat") {
		historyPeer, peerErr := nativeInputPeer(peer)
		if peerErr == nil {
			history, historyErr := api.MessagesGetHistory(reqCtx, &tg.MessagesGetHistoryRequest{
				Peer:     historyPeer,
				OffsetID: messageID + 1,
				Limit:    3,
			})
			if historyErr == nil {
				doc, err = nativeDocumentFromMessages(history, messageID)
			} else {
				err = classifyNativeReadError(historyErr)
			}
		}
	}
	if err != nil {
		return NativeFileLocation{}, err
	}
	refreshed := NativeFileLocation{
		PeerID:        task.PeerID,
		MessageID:     task.MessageID,
		Kind:          coalesce(task.NativeFile.Kind, task.Kind),
		FileID:        strconv.FormatInt(doc.ID, 10),
		AccessHash:    strconv.FormatInt(doc.AccessHash, 10),
		FileReference: base64.StdEncoding.EncodeToString(doc.FileReference),
		DCID:          doc.DCID,
		Size:          max64(doc.Size, task.Size),
		MimeType:      coalesce(task.NativeFile.MimeType, coalesce(task.ContentType, doc.MimeType)),
		FileName:      coalesce(task.NativeFile.FileName, task.FileName),
		UpdatedAt:     now(),
	}
	a.updateTask(task.ID, func(t *Task) {
		t.NativeFile = refreshed
		t.Error = ""
		t.UpdatedAt = now()
	})
	return refreshed, nil
}

func nativeInputPeer(peer NativePeerLocation) (tg.InputPeerClass, error) {
	id, err := strconv.ParseInt(peer.ID, 10, 64)
	if err != nil || id == 0 {
		return nil, fmt.Errorf("invalid native peer id: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(peer.Type)) {
	case "user":
		accessHash, parseErr := strconv.ParseInt(peer.AccessHash, 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid native user access hash: %w", parseErr)
		}
		return &tg.InputPeerUser{UserID: id, AccessHash: accessHash}, nil
	case "chat":
		return &tg.InputPeerChat{ChatID: id}, nil
	default:
		return nil, fmt.Errorf("unsupported native peer type %q", peer.Type)
	}
}

func nativeDocumentFromMessages(result tg.MessagesMessagesClass, messageID int) (*tg.Document, error) {
	modified, ok := result.AsModified()
	if !ok {
		return nil, fmt.Errorf("unexpected messages result: %T", result)
	}
	for _, item := range modified.GetMessages() {
		message, ok := item.(*tg.Message)
		if !ok || message.ID != messageID {
			continue
		}
		media, ok := message.GetMedia()
		if !ok {
			return nil, errors.New("消息已存在，但没有媒体文件")
		}
		docMedia, ok := media.(*tg.MessageMediaDocument)
		if !ok {
			return nil, fmt.Errorf("消息媒体不是文档视频：%T", media)
		}
		doc, ok := docMedia.Document.(*tg.Document)
		if !ok || doc == nil {
			return nil, fmt.Errorf("消息文档类型不支持：%T", docMedia.Document)
		}
		return doc, nil
	}
	return nil, errors.New("你要访问的内容已被删除，或当前账号没有权限读取这条消息")
}

// R4.27：refreshNativeFileLocationFromMetadataURL 已删除——它是 Go→Node→Go 的
// 自环回退（Node 的元数据接口内部仍调 Go 原生 API），只负责把真实错误包装成
// 「metadata refresh url is empty」。刷新失败现在直接透传 Go 侧真实原因。

// isGoBlobSourceURL 判断 SourceURL 是否指向 Go 自己的 blob 端点。
// M4.1 删除 Node 侧媒体桥后，goBlobSourceUrl（server/src/telegramService.js）把
// http-bridge 任务的源指向 `/api/accounts/{id}/blob?...`——同一实现的自环源。
func isGoBlobSourceURL(sourceURL string) bool {
	return strings.Contains(sourceURL, "/api/accounts/") && strings.Contains(sourceURL, "/blob?")
}

func (a *App) taskTransport(task Task) string {
	transport := task.Transport
	if transport == "" {
		transport = a.config.Transport
	}
	normalized := normalizeTransport(transport)
	// R4.23：blob 自环源的 http-bridge 任务一律升级回 native-mtproto。
	// 自环源只会把「账号未就绪」二次包装成 `source returned 404` 终态错误，
	// 让已恢复的账号永远拉不起任务（2.6.0 实测后台缓存全挂的根因）。
	// 存量任务（2.5.x/2.6.0 落盘）与旧客户端创建的任务都在这里被兜住。
	if normalized == "http-bridge" && isGoBlobSourceURL(task.SourceURL) {
		return "native-mtproto"
	}
	return normalized
}

func (a *App) throttle(windowBytes int64, windowStart time.Time, cancel <-chan struct{}) error {
	limit := a.config.RateLimitBps
	if limit <= 0 {
		return nil
	}
	expected := time.Duration(float64(windowBytes) / float64(limit) * float64(time.Second))
	sleepFor := expected - time.Since(windowStart)
	if sleepFor <= 0 {
		return nil
	}
	timer := time.NewTimer(sleepFor)
	defer timer.Stop()
	select {
	case <-cancel:
		return errCancelled
	case <-timer.C:
		return nil
	}
}

func (a *App) taskSnapshot(id string) *Task {
	a.mu.Lock()
	defer a.mu.Unlock()
	task := a.tasks[id]
	if task == nil {
		return nil
	}
	copy := *task
	return &copy
}

func (a *App) updateTask(id string, update func(*Task)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if task := a.tasks[id]; task != nil {
		update(task)
		_ = a.saveLocked()
	}
}

func (a *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"version":       version,
		"schemaVersion": storeSchemaVersion,
		"pid":           os.Getpid(),
		"uptime":        int(time.Since(a.startedAt).Seconds()),
		"taskCount":     len(a.tasks),
		"running":       len(a.running),
		"accounts":      a.accountsSummaryLocked(),
		"config":        a.config,
		"proxy":         a.proxy.status(),
	})
}

func (a *App) handleState(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, http.StatusOK, a.stateLocked())
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.mu.Lock()
	previousProxyURL := a.config.ProxyURL
	a.config = sanitizeConfig(applyConfigPatch(a.config, patch))
	// R4.23：移除「账号未就绪就把全局 Transport 降级为 http-bridge」的逻辑。
	// 根因（2.6.0 实测）：启动期 Node 回推配置时账号常未就绪，全局配置被降级成
	// http-bridge，此后新建任务全部带 http-bridge + Go blob 自环源；账号未就绪时
	// blob 404、错误被当成终态，下载/后台缓存「还是不行」。现在 R4.22 语义下
	// 任务本来就该等账号恢复（taskCanStartLocked 一票否决），降级只会制造自环任务。
	a.config.UpdatedAt = now()
	nextProxyURL := a.config.ProxyURL
	err := a.saveLocked()
	a.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// 代理改动立即生效：dialer 在 newTelegramClient 时读取，媒体 transport 每次请求读取。
	if _, reason := a.proxy.apply(nextProxyURL); reason != "" {
		log.Printf("网络代理配置无效：%s", reason)
	}
	log.Printf("%s", a.proxy.describeProxyConfig())
	if nextProxyURL != previousProxyURL {
		// 在途登录把旧 dialer 固定在 gotd client 里，不取消就会继续对旧代理拨号重试，
		// 并在结束时把账号状态覆盖成失败。代理一变，这些流程就该让位给重新发起的登录。
		a.mu.Lock()
		cancelled := a.cancelLoginsLocked("", "")
		a.mu.Unlock()
		if cancelled > 0 {
			log.Printf("网络代理已变更，取消 %d 个进行中的登录流程以改用新代理", cancelled)
		}
	}
	go a.pumpOnce()
	// 配置每次同步都触发全量健康检查（per-account 去重兜底）：
	// 2.4.5 实测缺口——应用重启时 Node 回推的代理 URL 与存储值相同，
	// 若只在「URL 变更」时重检，重启后 failed 账号要干等最长 30 分钟巡检。
	// 启动期 Node 会在 Go 就绪后立即 PUT 一次配置，这里顺带覆盖了「开机即重检」。
	go a.scheduleAllHealthChecks("配置同步重检")
	writeJSON(w, http.StatusOK, a.snapshot())
}

func (a *App) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.mu.Lock()
		defer a.mu.Unlock()
		writeJSON(w, http.StatusOK, a.listTasksLocked())
	case http.MethodPost:
		var input Task
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.mu.Lock()
		task := a.upsertTaskLocked(input)
		err := a.saveLocked()
		a.mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		go a.pumpOnce()
		writeJSON(w, http.StatusOK, task)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (a *App) handleNativeAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.mu.Lock()
		defer a.mu.Unlock()
		writeJSON(w, http.StatusOK, a.publicNativeAccountsLocked())
	case http.MethodPost, http.MethodPut:
		var input NativeAccount
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if input.UserID == "" || input.AccountID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "userId and accountId are required"})
			return
		}
		a.mu.Lock()
		account, err := a.upsertNativeAccountLocked(input)
		if err == nil {
			err = a.saveNativeLocked()
		}
		a.mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, publicNativeAccount(account))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (a *App) handleNativeAccount(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/native/accounts/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected /api/native/accounts/:userId/:accountId"})
		return
	}
	userID, err := url.PathUnescape(parts[0])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	accountID, err := url.PathUnescape(parts[1])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	action := ""
	if len(parts) >= 3 {
		action = parts[2]
	}
	a.mu.Lock()
	account := a.native[nativeAccountKey(userID, accountID)]
	if account == nil {
		a.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "native account is not prepared"})
		return
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		defer a.mu.Unlock()
		writeJSON(w, http.StatusOK, publicNativeAccount(*account))
	case r.Method == http.MethodDelete && action == "":
		// 清理账号记录（登出/残留记录清理用）：先取消在途登录，再摘除并落盘。
		if superseded := a.cancelLoginsLocked(userID, accountID); superseded > 0 {
			log.Printf("清理账号 %s/%s 前取消了 %d 个在途登录", userID, accountID, superseded)
		}
		removed := *account
		delete(a.native, nativeAccountKey(userID, accountID))
		if err := a.saveNativeLocked(); err != nil {
			a.mu.Unlock()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		a.mu.Unlock()
		// R4.29：账号记录已删（登出/清理），常驻媒体连接一并回收，
		// 避免旧 session 的连接继续占用或在 relogin 后撞 AUTH_KEY 冲突。
		a.dropMediaConn(nativeAccountKey(userID, accountID))
		log.Printf("已删除 Go 原生账号记录 %s/%s", userID, accountID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": publicNativeAccount(removed)})
	case r.Method == http.MethodPost && action == "health":
		snapshot := *account
		a.mu.Unlock()
		checked, err := a.nativeHealthCheck(snapshot)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, publicNativeAccount(checked))
	case r.Method == http.MethodPost && action == "login" && len(parts) >= 4 && parts[3] == "start":
		a.mu.Unlock()
		var input struct {
			Phone   string `json:"phone"`
			APIID   int    `json:"apiId"`
			APIHash string `json:"apiHash"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := a.startNativeLogin(userID, accountID, input.Phone, input.APIID, input.APIHash)
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
	case r.Method == http.MethodPost && action == "login" && len(parts) >= 4 && parts[3] == "qr-start":
		a.mu.Unlock()
		var input struct {
			APIID   int    `json:"apiId"`
			APIHash string `json:"apiHash"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := a.startNativeQRLogin(userID, accountID, input.APIID, input.APIHash)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	case r.Method == http.MethodPost && action == "login" && len(parts) >= 4 && parts[3] == "qr-status":
		a.mu.Unlock()
		var input struct {
			LoginID string `json:"loginId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := a.pollNativeQRLogin(input.LoginID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	case r.Method == http.MethodPost && action == "login" && len(parts) >= 4 && (parts[3] == "code" || parts[3] == "password"):
		a.mu.Unlock()
		var input struct {
			LoginID  string `json:"loginId"`
			Code     string `json:"code"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := a.continueNativeLogin(input.LoginID, parts[3], input.Code, input.Password)
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
	default:
		a.mu.Unlock()
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (a *App) handleTask(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	action := ""
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		id, action = parts[0], parts[1]
	}
	a.mu.Lock()
	task, ok := a.tasks[id]
	if !ok {
		a.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	switch {
	case r.Method == http.MethodGet:
	case r.Method == http.MethodDelete:
		if cancel := a.running[id]; cancel != nil {
			close(cancel)
			delete(a.running, id)
		}
		delete(a.tasks, id)
	case r.Method == http.MethodPost && action == "cancel":
		if cancel := a.running[id]; cancel != nil {
			close(cancel)
			delete(a.running, id)
		}
		task.Status = "cancelled"
		task.SpeedBps = 0
		task.Error = ""
		task.RetryAfter = 0
		task.UpdatedAt = now()
	case r.Method == http.MethodPost && action == "queue":
		// R4.25：「开始」语义 = 强制重启。若该任务仍有活着的下载 goroutine
		//（例如媒体路径挂死），必须先取消并清掉 running 坑，否则 pumpOnce
		// 会因 running[id] 已存在而静默跳过——用户看到的就是「点开始没反应」。
		if cancel := a.running[id]; cancel != nil {
			close(cancel)
			delete(a.running, id)
			log.Printf("task %s queued: previous live goroutine cancelled (force restart)", id)
		}
		task.Status = "queued"
		task.SpeedBps = 0
		task.Error = ""
		task.RetryCount = 0
		task.RetryAfter = 0
		task.UpdatedAt = now()
	default:
		a.mu.Unlock()
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	err := a.saveLocked()
	result := *task
	a.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	go a.pumpOnce()
	writeJSON(w, http.StatusOK, result)
}

func (a *App) snapshot() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked()
}

func (a *App) stateLocked() map[string]any {
	tasks := a.listTasksLocked()
	counts := map[string]int{}
	var speed int64
	for _, task := range tasks {
		counts[task.Status]++
		if task.Status == "downloading" || task.Status == "running" {
			speed += task.SpeedBps
		}
	}
	transport := normalizeTransport(a.config.Transport)
	strategy := "Go 下载服务已接管队列、断点、限速和落盘；媒体源统一走 Go 原生 MTProto。"
	nativeReady := a.nativeReadyLocked()
	readyAccountKeys := make([]string, 0)
	for _, account := range a.native {
		if nativeAccountEligible(*account) {
			readyAccountKeys = append(readyAccountKeys, nativeAccountKey(account.UserID, account.AccountID))
		}
	}
	sort.Strings(readyAccountKeys)
	native := map[string]any{
		"ready":            nativeReady,
		"readyAccountKeys": readyAccountKeys,
		"requiredPasses":   2,
		"status":           "needs-login",
		"note":             "Go 原生 MTProto 需扫码登录；登录后自动完成一次 Telegram 授权健康检查即就绪（R4.22 起不再需要多轮抽样验证）。",
	}
	if nativeReady {
		native["status"] = "healthy"
		native["note"] = "已有健康 Go 原生 MTProto session，可以灰度启用 native-mtproto。"
	}
	if transport == "native-mtproto" {
		strategy = "媒体源已统一走 Go 原生 MTProto；文件读取直接使用 Go session，FILE_REFERENCE_EXPIRED 会尝试刷新元数据后续传。"
	}
	// R4.25：调度诊断可见化。running 表可能含「幻影占坑」（任务状态已非下载中，
	// 但旧 goroutine 尚未退出），这正是 2.6.2 里「运行中 1/1 却全部排队中」的矛盾来源。
	// 因此分别上报：有效在跑数、坑位总数、坑位明细，前端可据此直接定位。
	runningActive := 0
	runningTaskIDs := make([]string, 0, len(a.running))
	for id := range a.running {
		runningTaskIDs = append(runningTaskIDs, id)
		if stored := a.tasks[id]; stored != nil && (stored.Status == "downloading" || stored.Status == "running") {
			runningActive++
		}
	}
	sort.Strings(runningTaskIDs)
	return map[string]any{
		"ok":             true,
		"version":        version,
		"schemaVersion":  storeSchemaVersion,
		"pid":            os.Getpid(),
		"uptime":         int(time.Since(a.startedAt).Seconds()),
		"dataDir":        a.dataDir,
		"config":         a.config,
		"counts":         counts,
		"running":        runningActive,
		"runningSlots":   len(a.running),
		"runningTaskIds": runningTaskIDs,
		"speedBps":       speed,
		"tasks":          tasks,
		"transport":      transport,
		"nativeMTProto":  native,
		"accounts":       a.accountsSummaryLocked(),
		// R4.29：媒体 DC 分级探测快照（账号 key → 快照），诊断页直接展示
		// 「代理放行了哪些 DC、没放行哪些」。
		"mediaProbes": a.mediaProbes,
		"strategy":    strategy,
		"proxy":       a.proxy.status(),
	}
}

// accountsSummaryLocked 在调用方已持 a.mu 锁的前提下，统计原生账号健康分布。
func (a *App) accountsSummaryLocked() map[string]any {
	byStatus := map[string]int{}
	total := 0
	healthy := 0
	ready := 0
	failed := 0
	degraded := 0
	for _, account := range a.native {
		total++
		status := normalizeNativeStatus(account.Status, account.Ready)
		byStatus[status]++
		if status == "healthy" {
			healthy++
		}
		// R4.8：就绪口径必须与调度/轮转（taskCanStartLocked）一致，统一走 nativeAccountEligible。
		// normalizeNativeStatus 在 Ready=true 时会无条件判 healthy，坏账号登出/失败后残留
		// Ready=true 会让 healthy 虚高——故另给 ready/failed 两组计数供外部监控直读。
		// 保留 healthy/byStatus 原口径以兼容既有前端消费，不在此处改变其语义。
		if nativeAccountEligible(*account) {
			ready++
		}
		if strings.TrimSpace(account.Status) == "failed" {
			failed++
		}
		// R4.11：Ready 但最近连续下载/健康检查失败的账号属「隐性退化」——
		// 还能通过就绪口径，但实际已在失败，监控侧应能直接看到。
		if nativeAccountEligible(*account) && account.ConsecutiveFailures > 0 {
			degraded++
		}
	}
	return map[string]any{
		"total":    total,
		"healthy":  healthy,
		"ready":    ready,
		"failed":   failed,
		"degraded": degraded,
		"byStatus": byStatus,
	}
}

func (a *App) listTasksLocked() []Task {
	tasks := make([]Task, 0, len(a.tasks))
	for _, task := range a.tasks {
		tasks = append(tasks, *task)
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].Order != tasks[j].Order {
			return tasks[i].Order < tasks[j].Order
		}
		return tasks[i].CreatedAt < tasks[j].CreatedAt
	})
	return tasks
}

func (a *App) upsertTaskLocked(input Task) Task {
	if input.ID == "" {
		input.ID = taskID(input.UserID, input.AccountID, input.PeerID, input.MessageID)
	}
	existing := a.tasks[input.ID]
	if existing == nil {
		created := now()
		input.CreatedAt = created
		input.UpdatedAt = created
		input.Status = coalesce(input.Status, "queued")
		input.Source = coalesce(input.Source, "manual")
		input.Order = input.OrderOrDefault(int64(len(a.tasks) + 1))
		if input.PartPath == "" && input.FilePath != "" {
			input.PartPath = input.FilePath + ".part"
		}
		a.tasks[input.ID] = &input
		return input
	}
	if input.FileName != "" {
		existing.FileName = input.FileName
	}
	if input.Size > 0 {
		existing.Size = input.Size
	}
	if input.ContentType != "" {
		existing.ContentType = input.ContentType
	}
	if input.Kind != "" {
		existing.Kind = input.Kind
	}
	if input.Source != "" {
		existing.Source = input.Source
	}
	if input.Transport != "" {
		existing.Transport = normalizeTransport(input.Transport)
	}
	existing.AutoCache = existing.AutoCache || input.AutoCache
	if input.SourceURL != "" {
		existing.SourceURL = input.SourceURL
	}
	if input.NativePeer.Type != "" || input.NativePeer.ID != "" {
		existing.NativePeer = input.NativePeer
	}
	if input.FilePath != "" {
		existing.FilePath = input.FilePath
	}
	if input.PartPath != "" {
		existing.PartPath = input.PartPath
	} else if existing.PartPath == "" && existing.FilePath != "" {
		existing.PartPath = existing.FilePath + ".part"
	}
	if input.InlineURL != "" {
		existing.InlineURL = input.InlineURL
	}
	if input.NativeFile.MessageID != 0 || input.NativeFile.FileID != "" || input.NativeFile.FileReference != "" {
		existing.NativeFile = input.NativeFile
		existing.NativeFile.UpdatedAt = coalesce(existing.NativeFile.UpdatedAt, now())
	}
	if input.Order > 0 {
		existing.Order = input.Order
	}
	// R4.26：终态任务的复活收敛。此前 ensure（POST /api/tasks）每命中一次
	// 就把 error/cancelled 任务静默改回 queued 并清零退避——上游以任何频率
	// 轮询都会形成「复活→失败→再复活」的无退避紧循环（2.6.3 实测日志风暴）。
	// 现在分三类：
	//   - cancelled：仅显式来源（source != auto，即用户重新点下载）才复活；
	//     后台缓存的幂等轮询（source=auto）不得擅自复活用户取消的任务；
	//   - error：仍允许复活（前端重新点下载等价于重试），但不清零 RetryCount，
	//     瞬态退避上限继续生效；只有「开始」（queue 动作）才清零；
	//   - 有活 goroutine 的任务一律不复活（避免双 goroutine 写同一 part 文件）。
	if _, live := a.running[input.ID]; !live {
		revive := false
		switch existing.Status {
		case "error":
			revive = true
		case "cancelled":
			revive = input.Source != "" && input.Source != "auto"
		}
		if revive {
			existing.Status = "queued"
			existing.RetryAfter = 0
		}
	}
	existing.UpdatedAt = now()
	return *existing
}

func (a *App) upsertNativeAccountLocked(input NativeAccount) (NativeAccount, error) {
	key := nativeAccountKey(input.UserID, input.AccountID)
	existing := a.native[key]
	created := now()
	if existing == nil {
		existing = &NativeAccount{
			UserID:    input.UserID,
			AccountID: input.AccountID,
			Status:    "needs-relogin",
			CreatedAt: created,
		}
		a.native[key] = existing
	}
	if input.Phone != "" {
		existing.Phone = input.Phone
	}
	if input.DisplayName != "" {
		existing.DisplayName = input.DisplayName
	}
	if input.APIID > 0 {
		existing.APIID = input.APIID
	}
	if input.APIHash != "" {
		encrypted, err := a.encryptNativeSession([]byte(input.APIHash))
		if err != nil {
			return NativeAccount{}, err
		}
		existing.APIHash = encrypted
	}
	if input.Session != "" {
		encrypted, err := a.encryptNativeSession([]byte(input.Session))
		if err != nil {
			return NativeAccount{}, err
		}
		existing.Session = encrypted
		existing.Ready = false
		existing.HealthPasses = 0
		existing.Status = "session-imported"
		existing.Error = "Go session payload 已加密保存，等待 gotd 健康检查"
	}
	if input.Status != "" {
		existing.Status = normalizeNativeStatus(input.Status, input.Ready)
	}
	if input.Ready {
		existing.Ready = true
		// R4.22：就绪即健康，不再用「通过次数」做二次门槛。
		existing.HealthPasses = 2
		existing.Status = "healthy"
		existing.Error = ""
	}
	existing.UpdatedAt = now()
	return *existing, nil
}

func (a *App) publicNativeAccountsLocked() []map[string]any {
	accounts := make([]map[string]any, 0, len(a.native))
	for _, account := range a.native {
		accounts = append(accounts, publicNativeAccount(*account))
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		left := fmt.Sprint(accounts[i]["userId"], "/", accounts[i]["accountId"])
		right := fmt.Sprint(accounts[j]["userId"], "/", accounts[j]["accountId"])
		return left < right
	})
	return accounts
}

func (a *App) nativeReadyLocked() bool {
	for _, account := range a.native {
		if nativeAccountEligible(*account) {
			return true
		}
	}
	return false
}

func nativeAccountEligible(account NativeAccount) bool {
	// R4.7：此前只看 Ready/HealthPasses/Session/API，不看 Status 字符串。
	// 而 normalizeNativeStatus 在 Ready=true 时会无条件判 healthy，一旦坏账号残留
	// Ready=true（登出/失败后未清），就会被当就绪、进入"可用账号集合"，拖垮批量任务。
	// 这里显式排除 needs-relogin / failed。
	switch strings.TrimSpace(account.Status) {
	case "needs-relogin", "failed":
		return false
	}
	return account.Ready && account.HealthPasses >= 2 && account.Session != "" && account.APIID > 0 && account.APIHash != ""
}

func (t Task) OrderOrDefault(fallback int64) int64 {
	if t.Order > 0 {
		return t.Order
	}
	return fallback
}

func sanitizeConfig(input Config) Config {
	if input.Concurrency < 1 {
		input.Concurrency = 1
	}
	if input.Concurrency > 10 {
		input.Concurrency = 10
	}
	if input.RateLimitBps < 0 {
		input.RateLimitBps = 0
	}
	if input.PartSize <= 0 {
		input.PartSize = defaultPartSize
	}
	if input.Mode != "fast" {
		input.Mode = "conservative"
	}
	if input.Backend == "" {
		input.Backend = "go-sidecar"
	}
	input.Transport = normalizeTransport(input.Transport)
	// 代理地址只做去空白，合法性由 proxyRuntime.apply 判定并把原因写进 /health，
	// 保留用户原始输入便于前端回显与纠错。
	input.ProxyURL = strings.TrimSpace(input.ProxyURL)
	if input.UpdatedAt == "" {
		input.UpdatedAt = now()
	}
	return input
}

func applyConfigPatch(current Config, patch map[string]any) Config {
	if value, ok := boolValue(patch["enabled"]); ok {
		current.Enabled = value
	}
	if value, ok := intValue(patch["concurrency"]); ok {
		current.Concurrency = value
	}
	if value, ok := int64Value(patch["rateLimitBps"]); ok {
		current.RateLimitBps = value
	}
	if value, ok := stringValue(patch["mode"]); ok {
		current.Mode = value
	}
	if value, ok := int64Value(patch["partSize"]); ok {
		current.PartSize = value
	}
	if value, ok := stringValue(patch["transport"]); ok {
		current.Transport = value
	}
	if value, ok := stringValue(patch["proxyUrl"]); ok {
		current.ProxyURL = value
	}
	return current
}

func normalizeTransport(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	// M3.1：http-bridge 仅作为显式降级开关保留；其余取值（含空值）一律走 Go 原生 MTProto。
	case "http-bridge", "node-bridge", "bridge":
		return "http-bridge"
	case "native-mtproto", "go-mtproto", "gotd", "tdl":
		return "native-mtproto"
	default:
		return "native-mtproto"
	}
}

func publicNativeAccount(account NativeAccount) map[string]any {
	return map[string]any{
		"userId":       account.UserID,
		"accountId":    account.AccountID,
		"phone":        account.Phone,
		"displayName":  account.DisplayName,
		"apiId":        account.APIID,
		"apiSet":       account.APIID > 0 && account.APIHash != "",
		"status":       normalizeNativeStatus(account.Status, account.Ready),
		"ready":        nativeAccountEligible(account),
		"sessionSet":   account.Session != "",
		"error":        account.Error,
		"healthPasses": account.HealthPasses,
		"createdAt":    account.CreatedAt,
		"updatedAt":    account.UpdatedAt,
		"checkedAt":    account.CheckedAt,
	}
}

func normalizeNativeStatus(status string, ready bool) string {
	// R4.25：ready=false 时不得保留 "healthy"——「显示健康却不可调度」的
	// 自相矛盾中间态正是 R4.22 要消灭的东西，这里曾漏堵：2.5.x 落盘的
	// healthy+ready=false 记录会一路原样通过校验进入内存。
	if !ready && strings.TrimSpace(status) == "healthy" {
		return "needs-relogin"
	}
	if ready {
		return "healthy"
	}
	switch strings.TrimSpace(status) {
	case "session-imported", "needs-relogin", "code-sent", "password-needed", "qr-waiting", "checking", "failed":
		return status
	default:
		return "needs-relogin"
	}
}

func nativeAccountKey(userID, accountID string) string {
	return userID + "|" + accountID
}

func (a *App) encryptNativeSession(plain []byte) (string, error) {
	key, err := a.nativeSecretKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	cipherText := gcm.Seal(nil, nonce, plain, nil)
	return "v1:" + base64.StdEncoding.EncodeToString(nonce) + ":" + base64.StdEncoding.EncodeToString(cipherText), nil
}

func (a *App) decryptNativeSession(encoded string) ([]byte, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, session.ErrNotFound
	}
	parts := strings.Split(encoded, ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, errors.New("unsupported encrypted native payload")
	}
	key, err := a.nativeSecretKey()
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	cipherText, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, cipherText, nil)
}

func (a *App) nativeAPIHash(account NativeAccount) (string, error) {
	if account.APIHash == "" {
		return "", nil
	}
	plain, err := a.decryptNativeSession(account.APIHash)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

type nativeSessionStorage struct {
	app       *App
	userID    string
	accountID string
}

func (s nativeSessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	s.app.mu.Lock()
	account := s.app.native[nativeAccountKey(s.userID, s.accountID)]
	encoded := ""
	if account != nil {
		encoded = account.Session
	}
	s.app.mu.Unlock()
	if encoded == "" {
		return nil, session.ErrNotFound
	}
	return s.app.decryptNativeSession(encoded)
}

func (s nativeSessionStorage) StoreSession(ctx context.Context, data []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	encrypted, err := s.app.encryptNativeSession(data)
	if err != nil {
		return err
	}
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	account := s.app.native[nativeAccountKey(s.userID, s.accountID)]
	if account == nil {
		account = &NativeAccount{
			UserID:    s.userID,
			AccountID: s.accountID,
			Status:    "session-imported",
			CreatedAt: now(),
		}
		s.app.native[nativeAccountKey(s.userID, s.accountID)] = account
	}
	account.Session = encrypted
	account.UpdatedAt = now()
	if account.Status == "" || account.Status == "needs-relogin" {
		account.Status = "session-imported"
	}
	return s.app.saveNativeLocked()
}

func (a *App) nativeAccountSnapshot(userID, accountID string) (NativeAccount, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nativeAccountSnapshotLocked(userID, accountID)
}

func (a *App) nativeAccountSnapshotLocked(userID, accountID string) (NativeAccount, error) {
	account := a.native[nativeAccountKey(userID, accountID)]
	if account == nil {
		return NativeAccount{}, fmt.Errorf("Go 原生 MTProto 账号未准备好")
	}
	return *account, nil
}

// nativePrimaryDC 从落库的 gotd session 解析账号主 DC；0 表示未知或解析失败。
// R4.18：gotd 的 MediaOnly/DC 池会无条件 exportAuthorization(dc)，而 Telegram
// 对「导出到当前主 DC」直接返回 DC_ID_INVALID——媒体恰在主 DC 上时（2.5.2 实测
// 账号主 DC 5、抽样媒体也在 DC 5）必须复用主连接，不建池、不导出。
//
// R4.30：解析格式修正。gotd 的 session.Loader 落库的是嵌套包装
// {"Version":1,"Data":{"DC":N,...}}（见 gotd session.Loader.Save 的 jsonData），
// 此前按顶层 {"DC":N} 解析在生产环境恒得 0——健康探测全部跳过、R4.18 的
// 「媒体 DC == 主 DC 复用主连接」短路从未生效，2.6.7 实测复现 DC_ID_INVALID
// （export 到自己）与无谓的媒体 DC export FLOOD_WAIT。R4.18 的单测用手工平铺
// JSON 通过了，属于「测试夹具与生产数据形状不一致」——现已同时兼容两种形状，
// 并把单测改成用 gotd 真实嵌套形状构造。
//
// 判据为何可靠：落库 session 由 nativeSessionStorage（gotd 的会话回写回调）维护，
// 登录迁移/网络迁移后 gotd 会把当前 DC 回写进来；而媒体池连接的指纹比对只取
// AuthKeyID（见 sessionFingerprint），不会因回写盐值变化而误判换会话。
func (a *App) nativePrimaryDC(userID, accountID string) int {
	a.mu.Lock()
	account := a.native[nativeAccountKey(userID, accountID)]
	encoded := ""
	if account != nil {
		encoded = account.Session
	}
	a.mu.Unlock()
	if encoded == "" {
		return 0
	}
	raw, err := a.decryptNativeSession(encoded)
	if err != nil {
		return 0
	}
	var payload struct {
		DC   int `json:"DC"`
		Data struct {
			DC int `json:"DC"`
		} `json:"Data"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return 0
	}
	if payload.Data.DC > 0 {
		return payload.Data.DC
	}
	return payload.DC
}

// sessionFingerprint 提取会话的认证指纹（AuthKeyID，同一 AUTH_KEY 恒定不变）。
// 用途（R4.30）：媒体池常驻连接与主连接/健康检查共用同一 nativeSessionStorage，
// 任何一方 connect 握手成功都会回写 session（盐值/Salt 每次都变）——若按整个
// session 逐字节比对，常驻连接会在「自己回写之后」被下一次 acquire 误判成
// 「重新登录」而重建（2.6.7 实测：常驻连接存活 3 秒即被重建）。改比 AuthKeyID：
// 只有真正重新登录（新 AUTH_KEY）指纹才会变化。
// 解析失败返回 ""，调用方退回逐字节比较保持旧行为。
func (a *App) sessionFingerprint(encoded string) string {
	raw, err := a.decryptNativeSession(encoded)
	if err != nil {
		return ""
	}
	var payload struct {
		AuthKeyID []byte `json:"AuthKeyID"`
		Data      struct {
			AuthKeyID []byte `json:"AuthKeyID"`
		} `json:"Data"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	if len(payload.Data.AuthKeyID) > 0 {
		return string(payload.Data.AuthKeyID)
	}
	return string(payload.AuthKeyID)
}

func (a *App) newTelegramClient(account NativeAccount, apiHash string) (*telegram.Client, error) {
	if account.APIID <= 0 || apiHash == "" {
		return nil, errors.New("Go 原生 MTProto 缺少 API ID/Hash")
	}
	options := telegram.Options{
		SessionStorage:   nativeSessionStorage{app: a, userID: account.UserID, accountID: account.AccountID},
		NoUpdates:        true,
		MigrationTimeout: 30 * time.Second,
		RetryInterval:    time.Second,
		MaxRetries:       5,
	}
	if account.Session == "" {
		options.SessionStorage = nativeSessionStorage{app: a, userID: account.UserID, accountID: account.AccountID}
	}
	if dialer := a.proxy.dialer(); dialer != nil {
		// gotd 的默认 resolver 是 dcs.Plain(dcs.PlainOptions{})，其 Dial 退化为裸 net.Dialer，
		// 因此不显式注入就会直连 Telegram DC。这里只替换 Dial，
		// 其余默认值（Intermediate 传输协议、加密随机源、tcp、优先 IPv4）保持 gotd 原样。
		options.Resolver = dcs.Plain(dcs.PlainOptions{Dial: dialer})
	}
	return telegram.NewClient(account.APIID, apiHash, options), nil
}

func (a *App) nativeHealthCheck(account NativeAccount) (result NativeAccount, err error) {
	// R4.22：健康检查的开始与结束都留日志。此前存在「拿不到主 DC 就静默跳过探测」
	// 这类零输出路径——服务端日志里既没有 health probe 行、错误里也没有「分级探测：」
	// 前缀，用户无法判断诊断到底跑没跑（2.5.6 实测就卡在这里）。
	started := time.Now()
	proxyDesc := "直连"
	if a.proxy.dialer() != nil {
		proxyDesc = "经代理"
	}
	log.Printf("health check start: %s/%s（status=%s，出口=%s）", account.UserID, account.AccountID, coalesce(account.Status, "unknown"), proxyDesc)
	defer func() {
		log.Printf("health check end: %s/%s status=%s ready=%v 耗时 %s",
			account.UserID, account.AccountID, coalesce(result.Status, "unknown"), result.Ready,
			time.Since(started).Round(time.Millisecond))
	}()
	account.CheckedAt = now()
	if account.Session == "" {
		account.Ready = false
		account.Status = "needs-relogin"
		account.Error = "Go 原生 MTProto session 尚未创建，请先执行 Go 重新登录"
		account.ConsecutiveFailures++
		return a.saveNativeAccount(account)
	}
	apiHash, err := a.nativeAPIHash(account)
	if err != nil {
		account.Ready = false
		account.Status = "failed"
		account.Error = err.Error()
		account.ConsecutiveFailures++
		_, _ = a.saveNativeAccount(account)
		return account, err
	}
	client, err := a.newTelegramClient(account, apiHash)
	if err != nil {
		account.Ready = false
		account.Status = "failed"
		account.Error = a.withNetworkHint(err)
		account.ConsecutiveFailures++
		return a.saveNativeAccount(account)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// R4.20：分级探测——先用与 MTProto 相同的代理出口对账号主 DC 做一次裸 TCP 拨号，
	// 把「代理链路不通」与「MTProto 握手/授权卡死」拆开，避免笼统的
	// context deadline exceeded 让用户无从下手（2.5.3/2.5.4 实测缺口）。
	probeDC := a.nativePrimaryDC(account.UserID, account.AccountID)
	probeAddr := primaryDCAddr(probeDC)
	probeOK := false
	var probeErr error
	probeDur := time.Duration(0)
	// R4.22：探测无论成功、失败还是「拿不到主 DC」都必须留下日志。
	// 此前 DC 未知时整块静默跳过，日志里既无 health probe 行、错误里也无「分级探测：」
	// 前缀，与「诊断功能没生效」无法区分（2.5.6 实测困惑点）。
	if probeAddr == "" {
		log.Printf("health probe: 跳过——无法确定 %s/%s 的主 DC（session 解析结果 %d）", account.UserID, account.AccountID, probeDC)
	} else {
		probeStart := time.Now()
		probeErr = a.probeTelegramTCP(probeAddr, healthProbeTimeout)
		probeOK = probeErr == nil
		probeDur = time.Since(probeStart)
		if probeOK {
			log.Printf("health probe: TCP %s (DC %d) OK %s，耗时 %s", probeAddr, probeDC, proxyDesc, probeDur)
		} else {
			log.Printf("health probe: TCP %s (DC %d) FAILED %s，耗时 %s：%v", probeAddr, probeDC, proxyDesc, probeDur, probeErr)
		}
	}
	// R4.17：健康检查分级——Auth().Status（真实授权 RPC）成功即账号可用；
	// 媒体抽样失败（如 DC_ID_INVALID / 媒体 DC 连不上）只降级记录原因，
	// 不再把整个账号打成 failed 堵死会话列表（2.5.1 实测案例）。
	// R4.22 简化：健康检查只做一件事——确认授权可用。
	// Auth().Status 是一次真实的 Telegram RPC，成功即同时证明「代理链路通 +
	// MTProto 握手通 + session 有效」，这三件事正是用户唯一关心的。
	// 原先在此之后还有一层「媒体抽样」（找缩略图、读 64KB、验证媒体池），
	// 它自 R4.17 起已不参与健康判定，却制造了 DC_ID_INVALID 等大量假故障，
	// 已整层删除；媒体层真出问题时，由下载任务的真实错误直接暴露。
	err = client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !status.Authorized {
			return errors.New("gotd session 未授权，请重新登录")
		}
		return nil
	})
	if err != nil {
		account.Ready = false
		account.HealthPasses = 0
		message := strings.ToUpper(err.Error())
		if strings.Contains(message, "AUTH_KEY_UNREGISTERED") || strings.Contains(message, "SESSION_REVOKED") || strings.Contains(message, "未授权") {
			account.Status = "needs-relogin"
		} else {
			account.Status = "failed"
		}
		account.Error = compactError(err)
		// R4.20：优先给出分级探测的精确结论（哪一层不通），比通用网络提示更可操作。
		if diag := healthStageDiagnosis(probeDC, probeAddr, probeOK, probeDur, probeErr, err, a.proxy.dialer() != nil); diag != "" {
			account.Error = diag + "；原始错误：" + account.Error
		} else if hint := networkHintFor(err, a.proxy.dialer() != nil); hint != "" && !strings.Contains(account.Error, hint) {
			// 连不上 DC 时把「是不是没代理」讲清楚，避免用户只看超时无从下手。
			account.Error = account.Error + " —— " + hint
		}
		// R4.11：健康检查失败计入连续失败（观测口径，供 /health 退化预警与账号卡展示）。
		account.ConsecutiveFailures++
		// R4.29：健康检查无论成败都异步补一轮媒体 DC 全量探测——「登录正常、下载 0 字节」
		// 时，这里能直接看到代理放行了哪些 DC、没放行哪些（结果进 /api/state 诊断页）。
		go a.probeMediaDCs(account)
		return a.saveNativeAccount(account)
	}
	// R4.22：授权 RPC 通过即账号健康——不再要求「连续 2 次通过」。
	// 旧口径下首次通过会把 HealthPasses 记成 1、Ready 仍为 false，而 Status 已是 healthy：
	// 前端显示「健康」、下载却报「账号尚未就绪」，还要再等一轮巡检才真正可用。
	// 这个自相矛盾的中间态正是用户反馈「为什么这么复杂」的一部分，已删除。
	account.LastSuccessAt = now()
	account.ConsecutiveFailures = 0
	account.HealthPasses = 2 // 保持 nativeAccountEligible 既有门槛语义（>= 2）
	account.Ready = true
	account.Status = "healthy"
	account.Error = "session 已授权（Telegram 授权 RPC 通过）"
	go a.probeMediaDCs(account)
	return a.saveNativeAccount(account)
}

func (a *App) saveNativeAccount(account NativeAccount) (NativeAccount, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := nativeAccountKey(account.UserID, account.AccountID)
	existing := a.native[key]
	if existing == nil {
		existing = &NativeAccount{UserID: account.UserID, AccountID: account.AccountID, CreatedAt: coalesce(account.CreatedAt, now())}
		a.native[key] = existing
	}
	existing.Phone = account.Phone
	existing.DisplayName = account.DisplayName
	existing.APIID = account.APIID
	if account.APIHash != "" {
		existing.APIHash = account.APIHash
	}
	existing.Status = normalizeNativeStatus(account.Status, account.Ready)
	existing.Ready = account.Ready
	if account.Session != "" {
		existing.Session = account.Session
	}
	existing.Error = account.Error
	existing.HealthPasses = account.HealthPasses
	existing.CheckedAt = account.CheckedAt
	existing.UpdatedAt = now()
	if err := a.saveNativeLocked(); err != nil {
		return *existing, err
	}
	return *existing, nil
}

func (a *App) finalizeNativeAuthorization(userID, accountID string) (NativeAccount, error) {
	for attempt := 0; attempt < 10; attempt++ {
		a.mu.Lock()
		account := a.native[nativeAccountKey(userID, accountID)]
		if account != nil && account.Session != "" {
			account.Ready = false
			account.HealthPasses = 0
			account.Status = "session-imported"
			account.Error = "Go 原生账号已授权，等待一次授权健康检查通过即就绪"
			account.CheckedAt = ""
			account.UpdatedAt = now()
			// R4.11：同手机号去重——登录入口（/api/auth/start、QR start）每次都由 Node
			// 生成全新 accountId，用户对同一号码重复「添加账号」就会积累多条记录；
			// 旧 session 在 Telegram 侧已被本次登录顶掉失效，旧记录只会在账号列表里
			// 以 failed/needs-relogin 的样子误导用户（真实环境实证：同号 healthy/failed
			// 双记录）。授权成功时把同号旧记录一并清理。
			pruned := 0
			if account.Phone != "" {
				for key, other := range a.native {
					if key != nativeAccountKey(userID, accountID) && other != nil &&
						other.UserID == userID && other.Phone == account.Phone {
						delete(a.native, key)
						pruned++
					}
				}
			}
			result := *account
			err := a.saveNativeLocked()
			a.mu.Unlock()
			if pruned > 0 {
				log.Printf("同手机号去重：清理 %d 条旧账号记录（%s/%s）", pruned, userID, accountID)
			}
			// R4.1：所有授权成功路径（手机登录/二维码）都收口于此，
			// 统一调度一次自动健康检查；去重在 schedule 内兜底。
			go a.scheduleAutoHealthCheck(userID, accountID, "登录成功")
			return result, err
		}
		a.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	return NativeAccount{}, errors.New("Telegram 已授权，但 gotd session 尚未持久化，请重新扫码")
}

// ensureNativeAccountLocked 在登录入口补建缺失的 native 账号记录（调用方持有 a.mu）。
//
// 首次登录（POST /api/auth/start、/api/auth/qr/start）的 accountId 由 Node 侧新生成，
// Go 侧必然没有记录；此前这里直接报 "native account is not prepared"，导致所有
// 全新账号登录必失败（E2E 因预先建过档而未暴露）。请求里已带手机号与 API 凭据，
// 直接建档即可；凭据不全时返回明确错误而不是含糊的 not prepared。
// 已有记录时原样返回（admin 重登链路凭据可能留空，靠存储值解密回填）。
func (a *App) ensureNativeAccountLocked(userID, accountID, phone string, apiID int, apiHash string) (*NativeAccount, error) {
	key := nativeAccountKey(userID, accountID)
	if account := a.native[key]; account != nil {
		return account, nil
	}
	if userID == "" || accountID == "" {
		return nil, errors.New("userId and accountId are required")
	}
	if apiID <= 0 || apiHash == "" {
		return nil, errors.New("apiId/apiHash is required")
	}
	account := &NativeAccount{
		UserID:    userID,
		AccountID: accountID,
		Phone:     phone,
		APIID:     apiID,
		Status:    "needs-relogin",
		CreatedAt: now(),
	}
	a.native[key] = account
	return account, nil
}

func (a *App) startNativeLogin(userID, accountID, phone string, apiID int, apiHash string) (nativeLoginResult, error) {
	if phone == "" {
		return nativeLoginResult{}, errors.New("phone is required")
	}
	a.mu.Lock()
	account, err := a.ensureNativeAccountLocked(userID, accountID, phone, apiID, apiHash)
	if err != nil {
		a.mu.Unlock()
		return nativeLoginResult{}, err
	}
	if apiID <= 0 {
		apiID = account.APIID
	}
	if apiHash == "" && account.APIHash != "" {
		var err error
		copy := *account
		a.mu.Unlock()
		apiHash, err = a.nativeAPIHash(copy)
		if err != nil {
			return nativeLoginResult{}, err
		}
		a.mu.Lock()
	}
	if apiID <= 0 || apiHash == "" {
		a.mu.Unlock()
		return nativeLoginResult{}, errors.New("apiId/apiHash is required")
	}
	if account.APIID != apiID {
		account.APIID = apiID
	}
	if apiHash != "" {
		encrypted, err := a.encryptNativeSession([]byte(apiHash))
		if err != nil {
			a.mu.Unlock()
			return nativeLoginResult{}, err
		}
		account.APIHash = encrypted
	}
	account.Phone = phone
	account.Ready = false
	account.Status = "needs-relogin"
	account.Error = ""
	account.UpdatedAt = now()
	_ = a.saveNativeLocked()
	// 同一账号重复点「登录」时先取消旧流程：旧流程持有旧 dialer（可能对应改前的网络代理），
	// 若放任并行，两边都会拨号，且旧流程收尾时会把账号状态覆盖成失败。
	if superseded := a.cancelLoginsLocked(userID, accountID); superseded > 0 {
		log.Printf("账号 %s/%s 仍有 %d 个登录流程在进行，已取消后重新开始", userID, accountID, superseded)
	}
	loginID := taskID("native-login", userID, accountID, phone, time.Now().UnixNano())
	// 必须有上限：前端超时放弃后 goroutine 不能永久重试 Telegram DC。
	ctx, cancel := context.WithTimeout(context.Background(), loginLifetime)
	login := &NativeLogin{
		ID:          loginID,
		UserID:      userID,
		AccountID:   accountID,
		Phone:       phone,
		APIID:       apiID,
		APIHash:     apiHash,
		Status:      "starting",
		Code:        make(chan string, 1),
		Password:    make(chan string, 1),
		Result:      make(chan nativeLoginResult, 2),
		StartResult: make(chan nativeLoginResult, 1),
		Cancel:      cancel,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	a.logins[loginID] = login
	a.mu.Unlock()

	go a.runNativeLogin(ctx, login)
	select {
	case result := <-login.StartResult:
		return result, result.Error
	case <-time.After(loginStartTimeout):
		// 走到这里说明前端拿不到 loginID，这个流程已经无法被继续：
		// 必须就地取消，否则它会一直对 Telegram DC 重试（代理不通时尤其明显）。
		cancel()
		a.mu.Lock()
		delete(a.logins, loginID)
		a.mu.Unlock()
		// 这类超时基本都出在网络出口上：直接把代理提示写进错误，
		// 用户才不用去翻日志猜「是不是没配代理」。
		return nativeLoginResult{}, fmt.Errorf(
			"发送 Telegram 验证码超时（%s）：%s", loginStartTimeout, a.proxyHintFor())
	}
}

func (a *App) continueNativeLogin(loginID, step, code, password string) (nativeLoginResult, error) {
	a.mu.Lock()
	login := a.logins[loginID]
	a.mu.Unlock()
	if login == nil {
		return nativeLoginResult{}, errors.New("Go 登录流程不存在或已过期")
	}
	switch step {
	case "code":
		if code == "" {
			return nativeLoginResult{}, errors.New("code is required")
		}
		login.Code <- code
	case "password":
		if password == "" {
			return nativeLoginResult{}, errors.New("password is required")
		}
		login.Password <- password
	default:
		return nativeLoginResult{}, errors.New("unknown login step")
	}
	select {
	case result := <-login.Result:
		if result.Done || result.Error != nil {
			a.mu.Lock()
			delete(a.logins, loginID)
			a.mu.Unlock()
		}
		return result, result.Error
	case <-time.After(60 * time.Second):
		return nativeLoginResult{}, errors.New("等待 Telegram 登录结果超时")
	}
}

func (a *App) startNativeQRLogin(userID, accountID string, apiID int, apiHash string) (nativeQRLoginResult, error) {
	a.mu.Lock()
	account, err := a.ensureNativeAccountLocked(userID, accountID, "", apiID, apiHash)
	if err != nil {
		a.mu.Unlock()
		return nativeQRLoginResult{}, err
	}
	if apiID <= 0 {
		apiID = account.APIID
	}
	if apiHash == "" && account.APIHash != "" {
		copy := *account
		a.mu.Unlock()
		var err error
		apiHash, err = a.nativeAPIHash(copy)
		if err != nil {
			return nativeQRLoginResult{}, err
		}
		a.mu.Lock()
		account = a.native[nativeAccountKey(userID, accountID)]
	}
	if account == nil {
		a.mu.Unlock()
		return nativeQRLoginResult{}, errors.New("native account is not prepared")
	}
	if apiID <= 0 || apiHash == "" {
		a.mu.Unlock()
		return nativeQRLoginResult{}, errors.New("apiId/apiHash is required")
	}
	if account.APIID != apiID {
		account.APIID = apiID
	}
	if apiHash != "" {
		encrypted, err := a.encryptNativeSession([]byte(apiHash))
		if err != nil {
			a.mu.Unlock()
			return nativeQRLoginResult{}, err
		}
		account.APIHash = encrypted
	}
	account.Ready = false
	account.Session = ""
	account.HealthPasses = 0
	account.Status = "qr-waiting"
	account.Error = ""
	account.UpdatedAt = now()
	_ = a.saveNativeLocked()
	snapshot := *account
	loginID := taskID("native-qr", userID, accountID, time.Now().UnixNano())
	login := &NativeQRLogin{
		ID:        loginID,
		UserID:    userID,
		AccountID: accountID,
		APIID:     apiID,
		APIHash:   apiHash,
		Status:    "starting",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	a.qrLogins[loginID] = login
	a.mu.Unlock()

	token, err := a.exportNativeQRToken(snapshot, apiHash)
	if err != nil {
		a.mu.Lock()
		delete(a.qrLogins, loginID)
		if account := a.native[nativeAccountKey(userID, accountID)]; account != nil {
			account.Status = "failed"
			account.Error = compactError(err)
			account.UpdatedAt = now()
			_ = a.saveNativeLocked()
		}
		a.mu.Unlock()
		return nativeQRLoginResult{}, err
	}
	a.mu.Lock()
	login = a.qrLogins[loginID]
	if login == nil {
		a.mu.Unlock()
		return nativeQRLoginResult{}, errors.New("QR 登录流程已取消")
	}
	login.Token = token.Token
	login.URL = qrLoginURL(token.Token)
	login.QRImage = qrPNGDataURL(login.URL)
	login.Expires = time.Unix(int64(token.Expires), 0)
	login.Status = "waiting-scan"
	login.UpdatedAt = time.Now()
	result := nativeQRLoginResult{
		LoginID: login.ID,
		URL:     login.URL,
		QRImage: login.QRImage,
		Status:  login.Status,
		Expires: login.Expires.Format(time.RFC3339),
	}
	a.mu.Unlock()
	return result, nil
}

func (a *App) pollNativeQRLogin(loginID string) (nativeQRLoginResult, error) {
	a.mu.Lock()
	login := a.qrLogins[loginID]
	if login == nil {
		a.mu.Unlock()
		return nativeQRLoginResult{}, errors.New("QR 登录流程不存在或已过期")
	}
	if login.Polling {
		result := nativeQRLoginSnapshot(login)
		a.mu.Unlock()
		return result, nil
	}
	login.Polling = true
	defer func() {
		a.mu.Lock()
		if current := a.qrLogins[loginID]; current != nil {
			current.Polling = false
		}
		a.mu.Unlock()
	}()
	snapshot, err := a.nativeAccountSnapshotLocked(login.UserID, login.AccountID)
	if err != nil {
		a.mu.Unlock()
		return nativeQRLoginResult{}, err
	}
	token := append([]byte(nil), login.Token...)
	apiHash := login.APIHash
	expires := login.Expires
	a.mu.Unlock()

	if len(token) == 0 || time.Now().After(expires.Add(-15*time.Second)) {
		fresh, err := a.exportNativeQRToken(snapshot, apiHash)
		if err != nil {
			return nativeQRLoginResult{}, err
		}
		a.mu.Lock()
		login := a.qrLogins[loginID]
		if login == nil {
			a.mu.Unlock()
			return nativeQRLoginResult{}, errors.New("QR 登录流程已取消")
		}
		login.Token = fresh.Token
		login.URL = qrLoginURL(fresh.Token)
		login.QRImage = qrPNGDataURL(login.URL)
		login.Expires = time.Unix(int64(fresh.Expires), 0)
		login.Status = "waiting-scan"
		login.UpdatedAt = time.Now()
		result := nativeQRLoginResult{LoginID: login.ID, URL: login.URL, QRImage: login.QRImage, Status: login.Status, Expires: login.Expires.Format(time.RFC3339)}
		a.mu.Unlock()
		return result, nil
	}

	client, err := a.newTelegramClient(snapshot, apiHash)
	if err != nil {
		return nativeQRLoginResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var account NativeAccount
	var response nativeQRLoginResult
	err = client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err == nil && status.Authorized {
			account, err = a.finalizeNativeAuthorization(snapshot.UserID, snapshot.AccountID)
			if err != nil {
				return err
			}
			response = nativeQRLoginResult{Account: account, LoginID: loginID, Status: "authorized", Done: true}
			return nil
		}
		result, err := client.API().AuthImportLoginToken(ctx, token)
		if err != nil {
			if strings.Contains(err.Error(), "AUTH_TOKEN_EXPIRED") {
				return err
			}
			if strings.Contains(err.Error(), "SESSION_PASSWORD_NEEDED") {
				return fmt.Errorf("QR 登录遇到两步验证，请临时使用验证码登录完成 Go session")
			}
			return err
		}
		switch value := result.(type) {
		case *tg.AuthLoginTokenSuccess:
			_ = value
			account, err = a.finalizeNativeAuthorization(snapshot.UserID, snapshot.AccountID)
			if err != nil {
				return err
			}
			response = nativeQRLoginResult{Account: account, LoginID: loginID, Status: "authorized", Done: true}
			return nil
		case *tg.AuthLoginTokenMigrateTo:
			log.Printf("native qr login requires DC migration to %d for %s/%s", value.DCID, snapshot.UserID, snapshot.AccountID)
			if err := client.MigrateTo(ctx, value.DCID); err != nil {
				return fmt.Errorf("QR 登录迁移到 Telegram DC %d 失败：%w", value.DCID, err)
			}
			imported, err := client.API().AuthImportLoginToken(ctx, value.Token)
			if err != nil {
				return fmt.Errorf("QR 登录迁移后导入 token 失败：%w", err)
			}
			success, ok := imported.(*tg.AuthLoginTokenSuccess)
			if !ok {
				return fmt.Errorf("QR 登录迁移后返回未知响应：%T", imported)
			}
			_ = success
			account, err = a.finalizeNativeAuthorization(snapshot.UserID, snapshot.AccountID)
			if err != nil {
				return err
			}
			response = nativeQRLoginResult{Account: account, LoginID: loginID, Status: "authorized", Done: true}
			return nil
		case *tg.AuthLoginToken:
			a.mu.Lock()
			if login := a.qrLogins[loginID]; login != nil {
				login.Token = value.Token
				login.URL = qrLoginURL(value.Token)
				login.QRImage = qrPNGDataURL(login.URL)
				login.Expires = time.Unix(int64(value.Expires), 0)
				login.Status = "waiting-scan"
				login.UpdatedAt = time.Now()
				response = nativeQRLoginResult{LoginID: login.ID, URL: login.URL, QRImage: login.QRImage, Status: login.Status, Expires: login.Expires.Format(time.RFC3339)}
			}
			a.mu.Unlock()
			return nil
		default:
			return fmt.Errorf("未知 QR 登录响应：%T", result)
		}
	})
	if err != nil {
		if strings.Contains(err.Error(), "AUTH_TOKEN_EXPIRED") {
			a.mu.Lock()
			if login := a.qrLogins[loginID]; login != nil {
				login.Expires = time.Time{}
				login.Status = "waiting-scan"
				login.Error = ""
				response = nativeQRLoginSnapshot(login)
			}
			a.mu.Unlock()
			return response, nil
		}
		if transientQRLoginError(err) {
			log.Printf("native QR login transient poll failure for %s: %v", loginID, err)
			a.mu.Lock()
			if login := a.qrLogins[loginID]; login != nil {
				login.Status = "waiting-scan"
				login.Error = ""
				login.UpdatedAt = time.Now()
				response = nativeQRLoginSnapshot(login)
			}
			a.mu.Unlock()
			return response, nil
		}
		a.mu.Lock()
		if login := a.qrLogins[loginID]; login != nil {
			login.Status = "error"
			login.Error = compactError(err)
			login.UpdatedAt = time.Now()
			response = nativeQRLoginResult{LoginID: login.ID, URL: login.URL, QRImage: login.QRImage, Status: login.Status, Error: login.Error, Expires: login.Expires.Format(time.RFC3339)}
		}
		a.mu.Unlock()
		if response.LoginID != "" {
			return response, nil
		}
		return nativeQRLoginResult{}, err
	}
	if response.Done {
		a.mu.Lock()
		delete(a.qrLogins, loginID)
		a.mu.Unlock()
	}
	return response, nil
}

func nativeQRLoginSnapshot(login *NativeQRLogin) nativeQRLoginResult {
	if login == nil {
		return nativeQRLoginResult{}
	}
	result := nativeQRLoginResult{
		LoginID: login.ID,
		URL:     login.URL,
		QRImage: login.QRImage,
		Status:  login.Status,
		Error:   login.Error,
	}
	if !login.Expires.IsZero() {
		result.Expires = login.Expires.Format(time.RFC3339)
	}
	return result
}

func transientQRLoginError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "engine was closed") ||
		strings.Contains(message, "rpcdorequest") ||
		strings.Contains(message, "retry limit reached") ||
		strings.Contains(message, "timeout") ||
		strings.Contains(message, "connection")
}

func (a *App) exportNativeQRToken(account NativeAccount, apiHash string) (*tg.AuthLoginToken, error) {
	client, err := a.newTelegramClient(account, apiHash)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var token *tg.AuthLoginToken
	err = client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err == nil && status.Authorized {
			return errors.New("Go 原生账号已授权，无需重新扫码")
		}
		result, err := client.API().AuthExportLoginToken(ctx, &tg.AuthExportLoginTokenRequest{
			APIID:     account.APIID,
			APIHash:   apiHash,
			ExceptIDs: []int64{},
		})
		if err != nil {
			return err
		}
		switch value := result.(type) {
		case *tg.AuthLoginToken:
			token = value
			return nil
		case *tg.AuthLoginTokenMigrateTo:
			appLog.Info("native QR export requires DC migration", slog.Int("dc", value.DCID), slog.String("user", account.UserID), slog.String("account", account.AccountID))
			if err := client.MigrateTo(ctx, value.DCID); err != nil {
				return fmt.Errorf("QR token 迁移到 Telegram DC %d 失败：%w", value.DCID, err)
			}
			imported, err := client.API().AuthImportLoginToken(ctx, value.Token)
			if err != nil {
				return fmt.Errorf("QR token 迁移后导入失败：%w", err)
			}
			if success, ok := imported.(*tg.AuthLoginTokenSuccess); ok {
				_ = success
				return errors.New("Go 原生账号已授权")
			}
			return fmt.Errorf("QR token 迁移后返回未知响应：%T", imported)
		case *tg.AuthLoginTokenSuccess:
			return errors.New("Go 原生账号已授权")
		default:
			return fmt.Errorf("未知 QR token 响应：%T", result)
		}
	})
	if err != nil {
		return nil, err
	}
	if token == nil || len(token.Token) == 0 {
		return nil, errors.New("Telegram 未返回 QR 登录 token")
	}
	return token, nil
}

func qrLoginURL(token []byte) string {
	return "tg://login?token=" + base64.RawURLEncoding.EncodeToString(token)
}

func qrPNGDataURL(value string) string {
	code, err := qr.Encode(value, qr.M)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
}

// loginLifetime 限制单次登录流程的最大存活时间。
// gotd 会对连不上的 Telegram DC 持续重试，若 context 永不过期，
// 前端超时放弃后 goroutine 仍会一直拨号（没配代理时尤其明显，日志与连接都会被刷满）。
const loginLifetime = 10 * time.Minute

// loginStartTimeout 是「发起登录 → 拿到验证码」这一步的等待上限。
// 超过它前端就拿不到 loginID，登录流程已无法被继续，必须取消。
const loginStartTimeout = 40 * time.Second

// cancelLoginsLocked 取消进行中的登录并返回数量，调用方需持有 a.mu。
// userID 为空表示取消全部；否则只取消该账号的登录流程。
func (a *App) cancelLoginsLocked(userID, accountID string) int {
	count := 0
	for id, login := range a.logins {
		if userID != "" && (login.UserID != userID || login.AccountID != accountID) {
			continue
		}
		if login.Cancel != nil {
			login.Cancel()
		}
		delete(a.logins, id)
		count++
	}
	return count
}

func (a *App) runNativeLogin(ctx context.Context, login *NativeLogin) {
	defer func() {
		// 流程结束即摘掉登记，避免 a.logins 随「前端没轮询结果」的登录流程堆积。
		a.mu.Lock()
		delete(a.logins, login.ID)
		a.mu.Unlock()
	}()
	account, err := a.nativeAccountSnapshot(login.UserID, login.AccountID)
	if err != nil {
		login.StartResult <- nativeLoginResult{Error: err, LoginID: login.ID}
		return
	}
	account.APIID = login.APIID
	client, err := a.newTelegramClient(account, login.APIHash)
	if err != nil {
		login.StartResult <- nativeLoginResult{Error: err, LoginID: login.ID}
		return
	}
	err = client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err == nil && status.Authorized {
			account, err = a.finalizeNativeAuthorization(login.UserID, login.AccountID)
			if err != nil {
				return err
			}
			login.StartResult <- nativeLoginResult{Account: account, LoginID: login.ID, Done: true}
			return nil
		}
		sent, err := client.Auth().SendCode(ctx, login.Phone, auth.SendCodeOptions{AllowAppHash: true})
		if err != nil {
			return err
		}
		codeHash, err := sentCodeHash(sent)
		if err != nil {
			return err
		}
		login.CodeHash = codeHash
		account.Phone = login.Phone
		account.Ready = false
		account.Status = "code-sent"
		account.Error = ""
		account, _ = a.saveNativeAccount(account)
		login.StartResult <- nativeLoginResult{Account: account, LoginID: login.ID, PhoneCodeHash: codeHash}
		code := ""
		select {
		case code = <-login.Code:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err = client.Auth().SignIn(ctx, login.Phone, code, codeHash)
		if errors.Is(err, auth.ErrPasswordAuthNeeded) {
			account.Status = "password-needed"
			account.Error = ""
			account, _ = a.saveNativeAccount(account)
			login.Result <- nativeLoginResult{Account: account, LoginID: login.ID, PasswordRequired: true}
			password := ""
			select {
			case password = <-login.Password:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err = client.Auth().Password(ctx, password)
		}
		if err != nil {
			return err
		}
		account, err = a.finalizeNativeAuthorization(login.UserID, login.AccountID)
		if err != nil {
			return err
		}
		login.Result <- nativeLoginResult{Account: account, LoginID: login.ID, Done: true}
		return nil
	})
	if err != nil {
		// 被同账号的新登录取代（或被代理变更取消）时，旧流程不能再写账号状态：
		// 否则会把新流程刚写入的进度/成功结果覆盖成「失败」。
		if errors.Is(ctx.Err(), context.Canceled) {
			log.Printf("登录流程 %s 已取消（可能被更新的登录流程接管），不再更新账号状态", login.ID)
			result := nativeLoginResult{Account: account, LoginID: login.ID, Error: err}
			select {
			case login.StartResult <- result:
			default:
			}
			return
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("登录流程超时（%s）未完成：%w", loginLifetime, err)
		}
		account.Error = compactError(err)
		if hint := networkHintFor(err, a.proxy.dialer() != nil); hint != "" && !strings.Contains(account.Error, hint) {
			// 登录是最常见的「连不上」入口：把「是不是没配代理」直接写进错误里，
			// 用户不用再去翻日志猜。
			account.Error = account.Error + " —— " + hint
		}
		account.Ready = false
		account.Status = "failed"
		account, _ = a.saveNativeAccount(account)
		result := nativeLoginResult{Account: account, LoginID: login.ID, Error: err}
		select {
		case login.StartResult <- result:
		default:
		}
		select {
		case login.Result <- result:
		default:
		}
	}
}

func sentCodeHash(sent tg.AuthSentCodeClass) (string, error) {
	if value, ok := sent.(interface{ GetPhoneCodeHash() string }); ok {
		if hash := value.GetPhoneCodeHash(); hash != "" {
			return hash, nil
		}
	}
	if value, ok := sent.(*tg.AuthSentCode); ok {
		return value.PhoneCodeHash, nil
	}
	return "", fmt.Errorf("Telegram 未返回 phone code hash：%T", sent)
}

// R4.27：FLOOD_WAIT 是 Telegram 的显式限流指令（420 + 等待秒数）。此前它只被
// 当成普通瞬态错误按指数退避处理（起步 5 秒、封顶 5 分钟），而真实限流常见
// 1400+ 秒——任务在限流窗口内反复撞墙，既刷日志又加深限流。现在解析秒数，
// 按 Telegram 的要求精确等待（封顶 floodWaitBackoffCap）。
type floodWaitError struct {
	Seconds int
	Err     error
}

func (e *floodWaitError) Error() string {
	return fmt.Sprintf("FLOOD_WAIT: Telegram 要求等待 %d 秒后重试", e.Seconds)
}

func (e *floodWaitError) Unwrap() error { return e.Err }

var floodWaitRe = regexp.MustCompile(`(?i)FLOOD_WAIT[_ (]*(\d+)`)

// floodWaitFromError 从错误链中解析 FLOOD_WAIT 的等待秒数；非限流错误返回 0。
func floodWaitFromError(err error) int {
	if err == nil {
		return 0
	}
	var typed *floodWaitError
	if errors.As(err, &typed) && typed.Seconds > 0 {
		return typed.Seconds
	}
	if match := floodWaitRe.FindStringSubmatch(err.Error()); match != nil {
		if seconds, convErr := strconv.Atoi(match[1]); convErr == nil && seconds > 0 {
			return seconds
		}
	}
	return 0
}

// floodWaitBackoffCap 限流等待的封顶值：Telegram 实际限流多为几分钟到 1 小时，
// 封顶 4 小时只为防极端值把任务冻结数天；期间任务留在 queued，等待原因可见。
const floodWaitBackoffCap = 4 * time.Hour

func classifyNativeReadError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "FILE_REFERENCE_EXPIRED"):
		return fmt.Errorf("FILE_REFERENCE_EXPIRED: 原生 fileReference 已过期，需要刷新消息元数据后自动续传")
	case strings.Contains(msg, "FLOOD_WAIT"):
		if match := floodWaitRe.FindStringSubmatch(msg); match != nil {
			if seconds, convErr := strconv.Atoi(match[1]); convErr == nil && seconds > 0 {
				return &floodWaitError{Seconds: seconds, Err: err}
			}
		}
		return fmt.Errorf("FLOOD_WAIT: Telegram 要求等待后重试：%w", err)
	case strings.Contains(msg, "_MIGRATE_"):
		return fmt.Errorf("DC_MIGRATE: Telegram 要求切换 DC 后重试：%w", err)
	case strings.Contains(msg, "DC_ID_INVALID"):
		return fmt.Errorf("DC_ID_INVALID: 媒体 DC 授权导出被拒（session 与 DC 状态可能不同步；账号本身可用，若持续出现请退出后重新登录）：%w", err)
	default:
		return err
	}
}

func migrationDC(err error) int {
	if err == nil {
		return 0
	}
	match := migrateRe.FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return 0
	}
	dc, parseErr := strconv.Atoi(match[1])
	if parseErr != nil || dc <= 0 {
		return 0
	}
	return dc
}

func (a *App) currentPartSize() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.config.PartSize
}

func (a *App) nativeSecretKey() ([]byte, error) {
	secretFile := strings.TrimSpace(os.Getenv("FEIGRAM_DOWNLOADER_SECRET_FILE"))
	if secretFile == "" {
		secretFile = filepath.Join(a.dataDir, "native-secret")
		if _, err := os.Stat(secretFile); errors.Is(err, os.ErrNotExist) {
			secret := make([]byte, 32)
			if _, err := io.ReadFull(rand.Reader, secret); err != nil {
				return nil, err
			}
			if err := os.WriteFile(secretFile, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
				return nil, err
			}
		}
	}
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(secret))))
	return sum[:], nil
}

func boolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		if typed == "true" {
			return true, true
		}
		if typed == "false" {
			return false, true
		}
	}
	return false, false
}

func intValue(value any) (int, bool) {
	if parsed, ok := int64Value(value); ok {
		return int(parsed), true
	}
	return 0, false
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case string:
		var parsed int64
		_, err := fmt.Sscan(typed, &parsed)
		return parsed, err == nil
	}
	return 0, false
}

func stringValue(value any) (string, bool) {
	if typed, ok := value.(string); ok && typed != "" {
		return typed, true
	}
	return "", false
}

func withJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func taskID(parts ...any) string {
	h := sha1.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(fmt.Sprint(part)))
		_, _ = h.Write([]byte("|"))
	}
	return "go_" + hex.EncodeToString(h.Sum(nil))[:24]
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func coalesce(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func complete(actualSize, expectedSize int64) bool {
	return actualSize > 0 && (expectedSize <= 0 || actualSize >= expectedSize)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func transientSourceError(err error) bool {
	if err == nil {
		return false
	}
	// R4.22：账号未就绪不是「媒体源故障」，但同样要按瞬态处理——账号恢复后
	// 任务必须能自动续传，而不是被记为终态错误、等用户手动重试。
	if errors.Is(err, errAccountNotReady) {
		return true
	}
	// R4.25：无进度挂死也是瞬态——按退避自动重试，网络/DC 路径恢复后自动续传。
	if errors.Is(err, errDownloadStalled) {
		return true
	}
	// R4.26：空响应（0 分块）同样是链路/账号层瞬态故障。
	if errors.Is(err, errEmptyMediaResponse) {
		return true
	}
	text := strings.ToLower(err.Error())
	markers := []string{
		"connection refused",
		"unexpected eof",
		"timeout",
		"timed out",
		// R4.26：context deadline exceeded（拨号/RPC 超时）是典型瞬态——
		// 此前不在表内，账号failed期间的反复拨号超时被当成终态错误，
		// 是「复活→失败→再复活」循环的燃料之一。
		"deadline exceeded",
		"connection reset",
		"connection closed",
		"broken pipe",
		"not connected",
		"source returned 408",
		"source returned 425",
		"source returned 429",
		"source returned 500",
		"source returned 502",
		"source returned 503",
		"source returned 504",
		"file_reference_expired",
		"flood_wait",
		"dc_migrate",
		"_migrate",
		// R4.30：peer 解析失败（找不到会话）不再一票终态——索引落盘 + 深翻分页后
		// 大多数场景下一轮自愈就能命中；真退群/删频道的任务由重试上限兜底转终态。
		"找不到会话",
	}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// maxTransientRetries 瞬态失败（媒体源不可达/网络抖动等）的自动重试上限。
// 退避封顶 5 分钟，120 次 ≈ 最多自动续传 10 小时；超过即转 error，
// 避免媒体源永久不可用（如账号已删除、Node 网关下线）的任务无限刷日志。
// 用户手动重试（重新入队）会把 RetryCount 归零，上限不影响手动恢复。
const maxTransientRetries = 120

func retryDelay(count int) time.Duration {
	if count < 1 {
		count = 1
	}
	delay := time.Duration(5*(1<<minInt(count-1, 5))) * time.Second
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func formatDuration(duration time.Duration) string {
	if duration < time.Minute {
		return fmt.Sprintf("%d 秒", int(duration.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(duration.Minutes()))
}

func compactError(err error) string {
	text := strings.TrimSpace(err.Error())
	if len(text) > 180 {
		return text[:180] + "..."
	}
	return text
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
