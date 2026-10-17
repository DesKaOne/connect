package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// auth stores a *network* JWT, not a client JWT. Provider mode exchanges this
// JWT for a distinct, restart-stable Client JWT for each provider slot.
func auth(opts docopt.Opts) {
	apiURL, err := opts.String("--api_url")
	if err != nil || apiURL == "" {
		apiURL = DefaultApiUrl
	}
	fmt.Print("Paste a fresh URnetwork dashboard Auth Code (input is visible): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, os.ErrClosed) && len(line) == 0 {
		Err.Printf("read auth code: %v", err)
		return
	}
	code := strings.TrimSpace(line)
	if code == "" {
		Err.Print("empty auth code; nothing saved")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	strategy := connect.NewClientStrategyWithDefaults(ctx)
	defer strategy.Close()
	api := connect.NewBringYourApi(ctx, strategy, apiURL)
	defer api.Close()
	result, err := api.AuthCodeLoginSync(&connect.AuthCodeLoginArgs{AuthCode: code})
	code = "" // discard plaintext auth code as soon as the request finishes
	if err != nil {
		Err.Printf("auth-code login failed: %v", err)
		return
	}
	if result == nil || result.Error != nil || result.ByJwt == "" {
		message := "empty auth response"
		if result != nil && result.Error != nil {
			message = result.Error.Message
		}
		Err.Printf("auth-code login refused: %s", message)
		return
	}
	claims, err := connect.ParseByJwtUnverified(result.ByJwt)
	if err != nil || claims.ClientId != (connect.Id{}) {
		Err.Print("auth result is not a network JWT; refusing to save")
		return
	}
	path := filepath.Join(".urnetwork", "jwt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		Err.Printf("create credential directory: %v", err)
		return
	}
	if err := os.WriteFile(path, []byte(result.ByJwt), 0600); err != nil {
		Err.Printf("save network JWT: %v", err)
		return
	}
	fmt.Printf("Network JWT saved to %s. Run connectctl provide to create individual provider clients.\n", path)
}
