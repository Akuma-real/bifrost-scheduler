// package scheduler 表示这个文件属于“应用层调度器”包。
//
// 应用层负责把配置文件、Bifrost API、领域规则串起来。
// 它不直接决定“好坏供应商怎么判断”，那个规则放在 internal/domain/scheduler。
package scheduler

// import 表示这个文件依赖哪些包。
//
// encoding/json：把 config.json 解析成 Go 结构体。
// fmt：包装错误信息。
// os：读取磁盘文件。
// domain：本项目领域层，里面定义配置结构和默认值规则。
import (
	"encoding/json"
	"fmt"
	"os"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
)

// LoadConfig 从磁盘读取 config.json，并转换成运行时配置。
//
// path 是配置文件路径。
// 返回值 domain.RuntimeConfig 是“已经补齐默认值、解析好时间长度”的配置。
// 返回值 error 表示读取、解析、校验过程中是否出错。
func LoadConfig(path string) (domain.RuntimeConfig, error) {
	// os.ReadFile 会一次性读取整个文件内容。
	// data 的类型是 []byte，也就是“字节列表”。
	data, err := os.ReadFile(path)
	if err != nil {
		// %w 会把原始错误包进去。
		// 这样上层既能看到“read config”，也能继续判断真正的底层错误。
		return domain.RuntimeConfig{}, fmt.Errorf("read config: %w", err)
	}

	// var cfg domain.Config 表示声明一个变量 cfg。
	// 此时它里面都是 Go 的零值，例如字符串是 ""，数字是 0。
	var cfg domain.Config

	// json.Unmarshal 把 JSON 文本填进 cfg 结构体。
	// &cfg 表示把 cfg 的地址传进去，因为 Unmarshal 需要修改 cfg。
	if err := json.Unmarshal(data, &cfg); err != nil {
		return domain.RuntimeConfig{}, fmt.Errorf("parse config: %w", err)
	}

	// NormalizeConfig 会补默认值并做校验。
	// 例如 window 没写就默认 15m，min_weight 没写就用默认值。
	return domain.NormalizeConfig(cfg)
}
