package isureal

import (
	"net/http"
	"os"
	"os/exec"

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
		imageDataStore.reset()
		// login_sessions は作り直されるので、覚えていたセッションも捨てる (他のリージョンの分は DB の確認で外れる)。
		knownSessions.Clear()
		// ip_bans は TRUNCATE された。アプリ内の BAN も捨てる (他のリージョンの分は localBanTTL で切れる)。
		h.IPBans.abuse.reset()
		// 画像の所有者の手がかり。id は再利用されないので他のリージョンに残っても誤らない (image_meta.go)。
		imageMetas.Clear()
		go loadInitialDataBoundary(h.DB)
		go refreshSearchNorms(h.DB)

		// Set language to the implementation language.
		// Agents optimizing this app: set agent, harness, and model to the names used.
		// For multiple models, use comma-separated names; use brain when no AI model is used.
		return c.JSON(http.StatusOK, initializeResponse{Language: "go", Agent: "claude-code,Codex", Harness: "claude-code,Codex", Model: "claude-opus-5-5,GPT-6"})
	}
}
