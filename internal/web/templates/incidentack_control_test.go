package templates

import (
	"strings"
	"testing"
	"time"
)

func TestAckControlAcknowledgedWithoutEmail(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	out := renderTo(t, ackControl(1, "host", 5, true, &at, ""))
	if !strings.Contains(out, "incident-ack-status") {
		t.Fatalf("подтверждённый инцидент должен рисовать статус: %s", out)
	}
	if strings.Contains(out, "incident-ack-form") {
		t.Fatalf("подтверждённый инцидент не должен рисовать форму подтверждения: %s", out)
	}
	if strings.Contains(out, "@") {
		t.Fatalf("без ackedByEmail email не должен появляться: %s", out)
	}
}

func TestAckControlAcknowledgedWithEmail(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	out := renderTo(t, ackControl(1, "host", 5, true, &at, "op@example.com"))
	if !strings.Contains(out, "op@example.com") {
		t.Fatalf("с ackedByEmail статус должен нести email подтвердившего: %s", out)
	}
}

func TestAckControlUnacknowledgedOperatorShowsButton(t *testing.T) {
	out := renderTo(t, ackControl(1, "host", 5, true, nil, ""))
	if !strings.Contains(out, "incident-ack-form") {
		t.Fatalf("оператор без подтверждения должен видеть форму: %s", out)
	}
	if !strings.Contains(out, incidentAckPath(1, "host", 5)) {
		t.Fatalf("форма должна вести на incidentAckPath: %s", out)
	}
	if strings.Contains(out, "incident-ack-status") {
		t.Fatalf("без подтверждения статус не должен рисоваться: %s", out)
	}
}

func TestAckControlUnacknowledgedNonOperatorRendersNothing(t *testing.T) {
	out := renderTo(t, ackControl(1, "host", 5, false, nil, ""))
	if strings.TrimSpace(out) != "" {
		t.Fatalf("не-оператор без подтверждения не должен видеть ничего: %q", out)
	}
}
