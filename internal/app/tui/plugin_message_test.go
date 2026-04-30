package tui

import (
	"testing"
	"time"
)

func TestSendPluginUserMessageStartsVisibleRun(t *testing.T) {
	a := newTestApp(t)
	a.footer = newFooterForTest()
	a.status = nil

	if err := a.SendPluginUserMessage("/skill:example", ""); err != nil {
		t.Fatalf("SendPluginUserMessage: %v", err)
	}
	deadline := time.After(time.Second)
	for {
		fake := a.sess.(*fakeSession)
		fake.mu.Lock()
		got := append([]string(nil), fake.sentTexts...)
		fake.mu.Unlock()
		if len(got) == 1 && got[0] == "/skill:example" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("sent texts = %#v, want /skill:example", got)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestSendPluginUserMessageQueuesFollowUp(t *testing.T) {
	a := newTestApp(t)

	if err := a.SendPluginUserMessage("next", "followUp"); err != nil {
		t.Fatalf("SendPluginUserMessage: %v", err)
	}

	fake := a.sess.(*fakeSession)
	if len(fake.followUps) != 1 || fake.followUps[0] != "next" {
		t.Fatalf("followUps = %#v, want next", fake.followUps)
	}
	if len(a.pendingQueue) != 1 || a.pendingQueue[0] != "next" {
		t.Fatalf("pendingQueue = %#v, want next", a.pendingQueue)
	}
}
