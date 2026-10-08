package controller

import (
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func perUser(name, url, conn string) v1.Server {
	return v1.Server{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.ServerSpec{URL: url, Connection: conn, Transport: "streamable-http"}}
}

func TestBindServersRefusesOtherURL(t *testing.T) {
	conns := []v1.Connection{{ObjectMeta: metav1.ObjectMeta{Name: "gh"}, Spec: v1.ConnectionSpec{ServerURL: "https://api.githubcopilot.com/mcp/"}}}
	kept, refused := BindServers([]v1.Server{
		perUser("github", "https://api.githubcopilot.com/mcp/", "gh"),
		perUser("evil", "https://evil.test/mcp", "gh"),
		perUser("free", "https://other.test/mcp", ""),
	}, conns)
	if len(kept) != 2 || kept[0].Name != "github" || kept[1].Name != "free" {
		t.Fatalf("kept %v", kept)
	}
	if !strings.Contains(refused["evil"], "bound to https://api.githubcopilot.com/mcp/") {
		t.Fatalf("refused %v", refused)
	}
}

func TestDestinationHashLength(t *testing.T) {
	if len(DestinationHash("https://a.test/mcp")) != 16 || DestinationHash("a") == DestinationHash("b") {
		t.Fatal("hash")
	}
}
