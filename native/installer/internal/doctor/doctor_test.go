package doctor

import (
	"strings"
	"testing"
)

func TestMaskArn(t *testing.T) {
	got := MaskArn("arn:aws:sts::111122223333:assumed-role/Admin/me")
	if strings.Contains(got, "111122223333") || !strings.Contains(got, "3333:assumed-role/Admin/me") {
		t.Errorf("MaskArn = %q", got)
	}
	if got := MaskArn("not-an-arn"); got != "not-an-arn" {
		t.Errorf("MaskArn changed a non-ARN: %q", got)
	}
}
