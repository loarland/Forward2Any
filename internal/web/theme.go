package web

import (
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/loarland/Forward2Any/internal/web/ui"
)

// 默认外观。改成 blue 是为了保持和以前一样的观感（原来那份 pico.min.css 就是 blue 构建）。
const (
	DefaultThemeColor = "blue"
	DefaultThemeMode  = "auto"
)

// palette 是一件可选配色。Name 对应 static/palettes/<Name>.css，由
// scripts/gen-palettes.py 从 Pico 官方主题抽出，只含主色族变量。
type palette struct {
	Name  string
	Label string
}

// palettes 是设置页下拉框的内容，顺序即展示顺序。
//
// 这张表是配色名的**唯一权威**：refreshTheme 拿它校验设置值，设置页拿它渲染选项，
// 单测拿它和 static/palettes/ 目录对账（两边不许有对方没有的）。所以新增配色要同时
// 放好文件并在这里登记，否则测试会拦下来。
var palettes = []palette{
	{"default", "Pico 默认"},
	{"blue", "蓝"},
	{"cyan", "青"},
	{"jade", "玉绿"},
	{"green", "绿"},
	{"lime", "青柠"},
	{"yellow", "黄"},
	{"amber", "琥珀"},
	{"orange", "橙"},
	{"pumpkin", "南瓜"},
	{"red", "红"},
	{"pink", "粉"},
	{"fuchsia", "品红"},
	{"purple", "紫"},
	{"violet", "紫罗兰"},
	{"indigo", "靛蓝"},
	{"slate", "石板"},
	{"zinc", "锌灰"},
	{"grey", "灰"},
	{"sand", "沙"},
}

// themeModes 是亮暗三档。auto 不写 data-theme，交给 Pico 跟随系统。
var themeModes = []palette{
	{"auto", "跟随系统"},
	{"light", "浅色"},
	{"dark", "深色"},
}

type themeChoice struct {
	Color string
	Mode  string
}

func validThemeColor(name string) bool {
	for _, p := range palettes {
		if p.Name == name {
			return true
		}
	}
	return false
}

func validThemeMode(name string) bool {
	for _, m := range themeModes {
		if m.Name == name {
			return true
		}
	}
	return false
}

// refreshTheme 把设置里的外观缓存进内存。
//
// 每个页面渲染都要用到它，不能每次都去查库 —— 和「是否仍是默认密码」一样的做法。
// 校验放在这里而不是只放在保存时：库里的值可能是手改的、或是旧版本留下的，
// 而它会直接进 <link href>。
func (s *Server) refreshTheme() {
	c := themeChoice{Color: DefaultThemeColor, Mode: DefaultThemeMode}
	settings, err := s.store.Settings()
	if err != nil {
		s.log.Error("读取设置失败，外观先用默认值", "err", err)
	} else {
		if validThemeColor(settings.ThemeColor) {
			c.Color = settings.ThemeColor
		}
		if validThemeMode(settings.ThemeMode) {
			c.Mode = settings.ThemeMode
		}
	}
	s.theme.Store(c)
}

func (s *Server) themeChoice() themeChoice {
	if v, ok := s.theme.Load().(themeChoice); ok {
		return v
	}
	return themeChoice{Color: DefaultThemeColor, Mode: DefaultThemeMode}
}

// paletteFiles 列出内嵌的配色文件，供单测和设置页对账用。
func paletteFiles() ([]string, error) {
	entries, err := fs.ReadDir(ui.StaticFS, "static/palettes")
	if err != nil {
		return nil, fmt.Errorf("读取配色目录: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ".css" {
			out = append(out, e.Name()[:len(e.Name())-len(".css")])
		}
	}
	sort.Strings(out)
	return out, nil
}
