package tray

import (
	"strings"
	"testing"
)

// statusTitle 托盘首行状态文案（disabled 菜单项）。
func TestStatusTitle(t *testing.T) {
	cases := map[bool]string{
		true:  "● 加速中",
		false: "○ 已直通",
	}
	for in, want := range cases {
		if got := statusTitle(in); got != want {
			t.Errorf("statusTitle(%v) = %q, want %q", in, got, want)
		}
	}
}

// validate 必填回调缺失时报错，齐全时通过。
func TestDepsValidate(t *testing.T) {
	base := func() Deps {
		return Deps{
			Version:     "test",
			UIURL:       "http://127.0.0.1:12800",
			OpenUI:      func() {},
			Quit:        func() {},
			ToggleAccel: func(bool) {},
			AccelState:  func() bool { return true },
		}
	}
	if err := base().validate(); err != nil {
		t.Fatalf("完整 Deps 不应报错: %v", err)
	}
	for name, mutate := range map[string]func(*Deps){
		"OpenUI": func(d *Deps) { d.OpenUI = nil },
		"Quit":   func(d *Deps) { d.Quit = nil },
		"UIURL":  func(d *Deps) { d.UIURL = "" },
		"Accel":  func(d *Deps) { d.ToggleAccel, d.AccelState = nil, nil },
	} {
		d := base()
		mutate(&d)
		if err := d.validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("缺 %s: 应报错含字段名, got %v", name, err)
		}
	}
}

// tooltip 汇总版本信息。
func TestTooltip(t *testing.T) {
	got := tooltip("1.2.3")
	if !strings.Contains(got, "light-github") || !strings.Contains(got, "1.2.3") {
		t.Errorf("tooltip = %q", got)
	}
}
