package main

// 控制实验：
//  0) CreateSession 返回 conv0 → 用 conv0 InitSession + prompt（应成功）
//  1) 同一 session + 新 convB → prompt（验证自定义 convID 是否可用）
//  2) 同一 session + conv0 再问暗号（验证 conv0 上下文是否保留）
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"edgeone2api/internal/upstream"
)

func chat(ctx context.Context, c *upstream.Client, sessionID, convID, text string, timeout time.Duration) (string, error) {
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	items := []upstream.ContentItem{{Type: "text", Text: text}}
	cs, err := c.StartChat(ctx2, sessionID, convID, items)
	if err != nil {
		return "", fmt.Errorf("start chat: %w", err)
	}
	defer cs.Cancel()
	res, err := c.StreamEvents(ctx2, cs, nil, 0)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

func main() {
	log.SetFlags(log.LstdFlags)
	ctx := context.Background()
	c := upstream.NewClient("https://deepseek-harness.edgeone.cool")

	conv0, sessionID, err := c.CreateSession(ctx, "minimal")
	if err != nil {
		log.Fatalf("create session: %v", err)
	}
	fmt.Printf("[0] session=%s conv0=%s\n", sessionID, conv0)

	// A. 用 conv0 InitSession（正常网关路径）
	if err := c.InitSession(ctx, sessionID, conv0); err != nil {
		fmt.Printf("[A] InitSession(conv0) 失败: %v\n", err)
	} else {
		fmt.Println("[A] InitSession(conv0) OK")
	}

	// B. conv0 记暗号
	t0 := time.Now()
	rb, err := chat(ctx, c, sessionID, conv0, "请记住暗号：苹果A。只回复：好的", 150*time.Second)
	fmt.Printf("[B] conv0 记暗号: %v err=%v reply=%q\n", time.Since(t0).Round(time.Second), err, trunc(rb, 60))

	// C. 同一 session + 新 convB 记另一个暗号
	convB := "conv-B-" + randHexN(10)
	t0 = time.Now()
	rc, err := chat(ctx, c, sessionID, convB, "请记住暗号：香蕉B。只回复：好的", 150*time.Second)
	fmt.Printf("[C] convB 记暗号: %v err=%v reply=%q\n", time.Since(t0).Round(time.Second), err, trunc(rc, 60))

	// D. conv0 问暗号
	t0 = time.Now()
	rd, err := chat(ctx, c, sessionID, conv0, "我之前告诉你的暗号是什么？只回答暗号本身", 150*time.Second)
	fmt.Printf("[D] conv0 问暗号: %v err=%v reply=%q\n", time.Since(t0).Round(time.Second), err, trunc(rd, 80))

	// E. convB 问暗号
	t0 = time.Now()
	re, err := chat(ctx, c, sessionID, convB, "我之前告诉你的暗号是什么？只回答暗号本身", 150*time.Second)
	fmt.Printf("[E] convB 问暗号: %v err=%v reply=%q\n", time.Since(t0).Round(time.Second), err, trunc(re, 80))

	fmt.Println("\n=== 结论 ===")
	fmt.Printf("B 成功=%v | C 成功=%v | D 答苹果=%v | E 答香蕉=%v\n",
		err == nil && rb != "", err == nil && rc != "", contains(rd, "苹果"), contains(re, "香蕉"))
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func contains(s, sub string) bool {
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func randHexN(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:n]
	}
	return hex.EncodeToString(b)[:n]
}
