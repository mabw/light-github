package rule

import "testing"

// ---- Normalize：steampp 字段归一化（spike-c 实证的各分支） ----

func TestNormalize_FixedIP(t *testing.T) {
	r := Normalize("github.com", "20.207.73.82", "")
	if r.Kind != KindFixedIP || r.Forward != "20.207.73.82" {
		t.Fatalf("公网 IP 应归一化为 FixedIP，得到 %+v", r)
	}
}

func TestNormalize_LoopbackIPDowngradesToDynamic(t *testing.T) {
	// spike-b 实训：hosts 被劫持时会出现 127.0.0.1，绝不能当出口
	if r := Normalize("github.com", "127.0.0.1", ""); r.Kind != KindDynamic {
		t.Fatalf("回环 IP 必须降级 Dynamic，得到 %+v", r)
	}
}

func TestNormalize_PrivateIPDowngradesToDynamic(t *testing.T) {
	if r := Normalize("example.com", "192.168.1.1", ""); r.Kind != KindDynamic {
		t.Fatalf("私有 IP 必须降级 Dynamic，得到 %+v", r)
	}
}

func TestNormalize_CNAME(t *testing.T) {
	r := Normalize("api.github.com", "githubapi.rmbgame.net", "")
	if r.Kind != KindCNAME || r.Forward != "githubapi.rmbgame.net" {
		t.Fatalf("其他域名应归一化为 CNAME 通道，得到 %+v", r)
	}
}

func TestNormalize_SelfForwardIsDynamic(t *testing.T) {
	if r := Normalize("resources.github.com", "resources.github.com", ""); r.Kind != KindDynamic {
		t.Fatalf("Forward 为自身应是 Dynamic，得到 %+v", r)
	}
	if r := Normalize("Resources.GitHub.com.", "resources.github.com", ""); r.Kind != KindDynamic {
		t.Fatalf("大小写与尾点差异应视为自身，得到 %+v", r)
	}
}

func TestNormalize_RelaySchemeDowngradesToDynamic(t *testing.T) {
	// spike-c 发现：huggingface.co → http://nl.mossimo.top:41080/ 是官方带宽真中转，不使用
	if r := Normalize("huggingface.co", "http://nl.mossimo.top:41080/", ""); r.Kind != KindDynamic {
		t.Fatalf("http:// scheme 中转必须降级 Dynamic，得到 %+v", r)
	}
}

func TestNormalize_FakeSNIDowngradesToDynamic(t *testing.T) {
	// 纯隧道无法改写客户端 SNI，带 fakeSNI 的规则降级
	if r := Normalize("githubusercontent.com", "23.235.37.133", "Github"); r.Kind != KindDynamic {
		t.Fatalf("fakeSNI 非空必须降级 Dynamic，得到 %+v", r)
	}
}

// ---- Table：域名匹配 ----

func TestMatch_Exact(t *testing.T) {
	tbl := NewTable([]Rule{{Domain: "github.com", Kind: KindFixedIP, Forward: "20.207.73.82"}})
	r, ok := tbl.Match("github.com")
	if !ok || r.Domain != "github.com" {
		t.Fatalf("精确匹配失败: %+v ok=%v", r, ok)
	}
}

func TestMatch_WildcardSuffix(t *testing.T) {
	tbl := NewTable([]Rule{{Domain: "*.githubusercontent.com", Kind: KindDynamic}})
	if _, ok := tbl.Match("raw.githubusercontent.com"); !ok {
		t.Fatal("*.suffix 应匹配子域名")
	}
	if _, ok := tbl.Match("deep.sub.githubusercontent.com"); !ok {
		t.Fatal("*.suffix 应匹配任意深度子域名")
	}
}

func TestMatch_WildcardDoesNotMatchBareSuffix(t *testing.T) {
	tbl := NewTable([]Rule{{Domain: "*.githubusercontent.com", Kind: KindDynamic}})
	if _, ok := tbl.Match("githubusercontent.com"); ok {
		t.Fatal("*.suffix 不应匹配裸后缀本身")
	}
}

func TestMatch_ExactWinsOverWildcard(t *testing.T) {
	tbl := NewTable([]Rule{
		{Domain: "*.github.com", Kind: KindDynamic},
		{Domain: "api.github.com", Kind: KindCNAME, Forward: "githubapi.rmbgame.net"},
	})
	r, ok := tbl.Match("api.github.com")
	if !ok || r.Kind != KindCNAME {
		t.Fatalf("精确规则应优先于通配: %+v", r)
	}
}

func TestMatch_CaseInsensitiveAndPortStripped(t *testing.T) {
	tbl := NewTable([]Rule{{Domain: "github.com", Kind: KindFixedIP}})
	if _, ok := tbl.Match("GITHUB.COM"); !ok {
		t.Fatal("匹配应大小写不敏感")
	}
	if _, ok := tbl.Match("github.com:443"); !ok {
		t.Fatal("匹配应剥离端口")
	}
}

func TestMatch_Miss(t *testing.T) {
	tbl := NewTable([]Rule{{Domain: "github.com", Kind: KindFixedIP}})
	if _, ok := tbl.Match("gitee.com"); ok {
		t.Fatal("未命中应返回 false")
	}
	// 子域不应命中父域精确规则
	if _, ok := tbl.Match("www.github.com"); ok {
		t.Fatal("父域精确规则不应匹配子域")
	}
}
