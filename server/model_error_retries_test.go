package server

import "testing"

func TestParseModelErrorRetries(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"0", 0, true},   // 关闭额外重试
		{"2", 2, true},   // 默认值
		{" 5 ", 5, true}, // 容忍空白
		{"10", 10, true},
		{"11", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseModelErrorRetries(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseModelErrorRetries(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestValidateModelErrorRetries(t *testing.T) {
	for _, n := range []int{0, 1, 2, maxModelErrorRetries} {
		if err := validateModelErrorRetries(n); err != nil {
			t.Errorf("validateModelErrorRetries(%d) = %v, want nil", n, err)
		}
	}
	for _, n := range []int{-1, maxModelErrorRetries + 1} {
		if err := validateModelErrorRetries(n); err == nil {
			t.Errorf("validateModelErrorRetries(%d) = nil, want error", n)
		}
	}
}
