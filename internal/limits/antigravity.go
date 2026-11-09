package limits

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrAntigravityClosed means no Antigravity language server is running.
var ErrAntigravityClosed = errors.New("antigravity is not running")

const (
	antigravityStatusPath  = "/exa.language_server_pb.LanguageServerService/GetUserStatus"
	antigravitySummaryPath = "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary"
)

// antigravityWindows maps the quota summary's window names to minutes.
var antigravityWindows = map[string]int{"5h": 300, "weekly": 10080}

// Effort and variant suffixes of one model share its quota.
var antigravityVariant = regexp.MustCompile(`-(high|medium|low|extra-low|tiered|thinking)$`)
var antigravityLabelVariant = regexp.MustCompile(`\s*\([^)]*\)$`)

// AntigravityCollector asks a running Antigravity for its quotas through the
// discovery files its language server writes for Antigravity's own CLI.
type AntigravityCollector struct {
	Store      *Store
	GeminiHome string
	Client     *http.Client
	Now        func() time.Time
}

type antigravityHub struct {
	HTTPPort  int    `json:"httpPort"`
	CSRFToken string `json:"csrfToken"`
	modified  time.Time
}

type antigravityStatus struct {
	UserStatus struct {
		Email string `json:"email"`
		// The server's name for the subscription tier, e.g. "Google AI Pro".
		UserTier struct {
			Name string `json:"name"`
		} `json:"userTier"`
		PlanStatus struct {
			PlanInfo struct {
				PlanName string `json:"planName"`
			} `json:"planInfo"`
		} `json:"planStatus"`
		CascadeModelConfigData struct {
			ClientModelConfigs []antigravityModel `json:"clientModelConfigs"`
		} `json:"cascadeModelConfigData"`
	} `json:"userStatus"`
}

type antigravityModel struct {
	Label     string `json:"label"`
	ModelID   string `json:"modelId"`
	QuotaInfo *struct {
		RemainingFraction *float64 `json:"remainingFraction"`
		ResetTime         string   `json:"resetTime"`
	} `json:"quotaInfo"`
}

// antigravitySummary is the quota summary: model groups that each share a
// weekly and a 5-hour bucket.
type antigravitySummary struct {
	Response struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Buckets     []struct {
				BucketID          string   `json:"bucketId"`
				Window            string   `json:"window"`
				RemainingFraction *float64 `json:"remainingFraction"`
				ResetTime         string   `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	} `json:"response"`
}

// AntigravityHome is Antigravity's per-user folder.
func AntigravityHome(geminiHome string) string {
	if geminiHome == "" {
		home, _ := os.UserHomeDir()
		geminiHome = filepath.Join(home, ".gemini")
	}
	return filepath.Join(geminiHome, "antigravity")
}

func (c *AntigravityCollector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *AntigravityCollector) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// Collect takes one reading from the running language server, if any.
func (c *AntigravityCollector) Collect(ctx context.Context) error {
	var status antigravityStatus
	hub, err := c.query(ctx, nil, antigravityStatusPath, &status)
	if err != nil {
		return err
	}
	now := c.now().UTC()
	user := status.UserStatus
	account := Account{ID: user.Email, Email: user.Email, Plan: firstNonEmpty(strings.TrimSpace(user.UserTier.Name), user.PlanStatus.PlanInfo.PlanName)}
	if err := c.Store.RecordAccount(ctx, Antigravity, "", account, now); err != nil {
		return err
	}
	// The quota summary reports each model group's weekly and 5-hour limits
	// together, exactly as Antigravity shows them. Older versions without it
	// fall back to the per-model quota, which names only the binding limit.
	var summary antigravitySummary
	if _, err := c.query(ctx, &hub, antigravitySummaryPath, &summary); err == nil && len(summary.Response.Groups) > 0 {
		return c.observeSummary(ctx, account, summary, now)
	}
	return c.observeModels(ctx, account, user.CascadeModelConfigData.ClientModelConfigs, now)
}

func (c *AntigravityCollector) observeSummary(ctx context.Context, account Account, summary antigravitySummary, now time.Time) error {
	for _, group := range summary.Response.Groups {
		for _, bucket := range group.Buckets {
			minutes, ok := antigravityWindows[bucket.Window]
			if !ok || bucket.BucketID == "" || bucket.ResetTime == "" {
				continue
			}
			resets, err := time.Parse(time.RFC3339Nano, bucket.ResetTime)
			if err != nil {
				continue
			}
			if err := c.Store.Observe(ctx, Observation{
				Provider: Antigravity, Account: account.ID, Plan: account.Plan, Key: bucket.BucketID,
				Label: group.DisplayName, WindowMinutes: minutes,
				UsedPercent: antigravityUsed(bucket.RemainingFraction), ResetsAt: resets, ObservedAt: now,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *AntigravityCollector) observeModels(ctx context.Context, account Account, models []antigravityModel, now time.Time) error {
	seen := map[string]bool{}
	for _, model := range models {
		quota := model.QuotaInfo
		if quota == nil || quota.ResetTime == "" || model.ModelID == "" {
			continue
		}
		key := antigravityVariant.ReplaceAllString(model.ModelID, "")
		if seen[key] {
			continue
		}
		seen[key] = true
		resets, err := time.Parse(time.RFC3339Nano, quota.ResetTime)
		if err != nil {
			continue
		}
		// Only the binding limit is reported here: the 5-hour window, or the
		// weekly one once that runs out. Each length keeps its own key, and a
		// weekly window in its last five hours is still weekly.
		minutes := 300
		weekly, err := c.Store.hasWindow(ctx, Antigravity, account.ID, key+":10080", resets)
		if err != nil {
			return err
		}
		if weekly || resets.Sub(now) > 5*time.Hour+resetTolerance {
			minutes = 10080
		}
		if err := c.Store.Observe(ctx, Observation{
			Provider: Antigravity, Account: account.ID, Plan: account.Plan, Key: key + ":" + strconv.Itoa(minutes),
			Label: antigravityLabelVariant.ReplaceAllString(model.Label, ""), WindowMinutes: minutes,
			UsedPercent: antigravityUsed(quota.RemainingFraction), ResetsAt: resets, ObservedAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// antigravityUsed converts a remaining fraction to the percent used; an
// exhausted quota omits the fraction.
func antigravityUsed(remaining *float64) float64 {
	if remaining == nil {
		return 100
	}
	return math.Round((1-*remaining)*10000) / 100
}

// query calls one language server method, on hub when given, else on the
// first discovered server that answers, and returns the hub it used.
func (c *AntigravityCollector) query(ctx context.Context, hub *antigravityHub, path string, out any) (antigravityHub, error) {
	hubs := []antigravityHub{}
	if hub != nil {
		hubs = append(hubs, *hub)
	} else {
		hubs = readAntigravityHubs(filepath.Join(AntigravityHome(c.GeminiHome), "daemon"))
	}
	if len(hubs) == 0 {
		return antigravityHub{}, ErrAntigravityClosed
	}
	body := []byte(`{"metadata":{"ideName":"antigravity","extensionName":"antigravity","locale":"en"}}`)
	var lastErr error = ErrAntigravityClosed
	for _, candidate := range hubs {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://127.0.0.1:"+strconv.Itoa(candidate.HTTPPort)+path, bytes.NewReader(body))
		if err != nil {
			return antigravityHub{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		req.Header.Set("X-Codeium-Csrf-Token", candidate.CSRFToken)
		res, err := c.client().Do(req)
		if err != nil {
			// Discovery files outlive the servers that wrote them.
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		if res.StatusCode != http.StatusOK || readErr != nil {
			lastErr = fmt.Errorf("antigravity %s: HTTP %d", path[strings.LastIndex(path, "/")+1:], res.StatusCode)
			continue
		}
		if err := json.Unmarshal(data, out); err != nil {
			lastErr = err
			continue
		}
		return candidate, nil
	}
	return antigravityHub{}, lastErr
}

// readAntigravityHubs lists the discovery files, newest first.
func readAntigravityHubs(dir string) []antigravityHub {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var hubs []antigravityHub
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "hub_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var hub antigravityHub
		if json.Unmarshal(data, &hub) != nil || hub.HTTPPort <= 0 || hub.CSRFToken == "" {
			continue
		}
		if info, err := entry.Info(); err == nil {
			hub.modified = info.ModTime()
		}
		hubs = append(hubs, hub)
	}
	sort.Slice(hubs, func(i, j int) bool { return hubs[i].modified.After(hubs[j].modified) })
	return hubs
}
