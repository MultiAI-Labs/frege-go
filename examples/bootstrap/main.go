// Command bootstrap performs a one-time Frege sign-in and prints the refresh
// token your service should store.
//
// There are no API keys yet, so a headless service still needs a human to sign
// in ONCE. Run this, read the six-digit code from your email, and save the
// refresh token it prints. From then on your bot uses that token with
// frege.NewRefreshingToken and never needs a human again (until the refresh
// token is revoked).
//
//	FREGE_BASE_URL=https://frege.io go run ./examples/bootstrap
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	frege "github.com/MultiAI-Labs/frege-go"
)

func main() {
	base := os.Getenv("FREGE_BASE_URL") // "" -> frege.DefaultBaseURL
	ctx := context.Background()
	in := bufio.NewReader(os.Stdin)

	email := prompt(in, "Email: ")
	if err := frege.SendMagicCode(ctx, base, email); err != nil {
		die("could not send the sign-in code: %v", err)
	}
	fmt.Println("A six-digit code was emailed to", email)

	code := prompt(in, "Code: ")
	sess, err := frege.VerifyMagicCode(ctx, base, email, code)
	if err != nil {
		die("could not verify the code: %v", err)
	}

	fmt.Println()
	fmt.Println("Signed in as", sess.User.Email)
	fmt.Println("Store this refresh token (it rotates on each use):")
	fmt.Println()
	fmt.Println("  " + sess.RefreshToken)
	fmt.Println()
	fmt.Println("Set it as FREGE_REFRESH_TOKEN for the telegrambot example.")
}

func prompt(in *bufio.Reader, label string) string {
	fmt.Print(label)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
