package isureal

import (
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/labstack/echo/v5"
)

type initializeResponse struct {
	Language string `json:"language"`
	Agent    string `json:"agent"`
	Harness  string `json:"harness"`
	Model    string `json:"model"`
}

func initializeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		cmd := exec.Command("../sql/init.sh")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		resetAt := time.Now() // ip_bans の TRUNCATE はここまでに確定している
		// ベンチが投稿した画像のファイルを、このサーバーと他のリージョンのサーバーの両方で捨てる
		// (resetImageStore はメモリの imageDataStore も空にする)。
		if err := resetImageStore(); err != nil {
			return err
		}
		resetPeerImageStores(h.Config.Region)
		// login_sessions は作り直されるので、覚えていたセッションも捨てる (他のリージョンの分は DB の確認で外れる)。
		knownSessions.Clear()
		// ip_bans は TRUNCATE された。アプリ内の BAN も捨てる (他のリージョンの分は localBanTTL で切れる)。
		h.IPBans.abuse.reset()
		// 前回のベンチの BAN を、このサーバーはすぐ、ほかのリージョンは次の読み直しで捨てる。
		// 読み直し終わるまで待ってから返すので、応答の直後に来たリクエストが古い BAN で 403 にならない (#41)。
		if err := h.IPBans.reloadNow(c.Request().Context(), h.DB); err != nil {
			return err
		}
		time.Sleep(ipBanPeerWait(resetAt, time.Now()))
		go loadInitialDataBoundary(h.DB)
		go loadProfileImageHints(h.DB)
		go refreshSearchNorms(h.DB)
		// ベンチで読む表と索引を DB のキャッシュに載せておく (prewarm.go)。応答は待たない。
		go prewarmDB(h.DB, "initialize")

		// Set language to the implementation language.
		// Agents optimizing this app: set agent, harness, and model to the names used.
		// For multiple models, use comma-separated names; use brain when no AI model is used.
		return c.JSON(http.StatusOK, initializeResponse{Language: "go", Agent: "claude-code,Codex", Harness: "claude-code,Codex", Model: "claude-opus-5-5,GPT-6"})
	}
}
