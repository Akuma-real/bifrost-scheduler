// package main 表示这个文件属于命令行程序本身。
//
// rotating_file.go 放在 cmd/bifrost-scheduler 目录下，所以它只服务这个命令，
// 不会被其他包直接导入。
package main

// import 表示“这个文件要使用哪些外部代码”。
//
// 这些都是 Go 标准库：
//   - fmt：拼接错误信息。
//   - os：打开、关闭、删除、重命名文件。
//   - filepath：处理文件路径。
//   - strconv：把字符串转成数字。
//   - strings：处理字符串大小写、空格、后缀。
//   - sync：提供锁，避免多个 goroutine 同时写日志文件时打架。
import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// rotatingFile 是“会自动轮转的日志文件”。
//
// struct 表示“把多个字段组合成一个新类型”。
// 这个类型实现了 Write 方法，所以它可以被当作 io.Writer 使用。
// 简单理解：只要别人往它里面写日志，它就负责判断文件是否太大、是否需要换新文件。
type rotatingFile struct {
	// mu 是互斥锁。
	// 如果将来有多个 goroutine 同时写日志，Lock/Unlock 能保证一次只有一个人在改文件。
	mu sync.Mutex

	// path 是当前日志文件路径，例如 logs/bifrost-scheduler.log。
	path string

	// maxBytes 是单个日志文件最大字节数。
	// int64 是 64 位整数，适合表示文件大小。
	maxBytes int64

	// maxBackups 是最多保留几个旧日志文件。
	// 例如 5 表示保留 .1 到 .5。
	maxBackups int

	// file 是当前打开的文件句柄。
	// *os.File 前面的 * 表示“指针”，也就是指向真正文件对象的位置。
	file *os.File

	// size 记录当前日志文件已经有多大。
	size int64
}

// newRotatingFile 创建一个 rotatingFile。
//
// Go 里常见命名习惯：
//   - newXXX 通常表示“创建一个 XXX”。
//   - 返回 (*rotatingFile, error) 表示：成功时给调用者一个文件对象，失败时给错误。
func newRotatingFile(path string, maxBytes int64, maxBackups int) (*rotatingFile, error) {
	// maxBytes 必须大于 0，否则永远不知道什么时候轮转。
	if maxBytes <= 0 {
		return nil, fmt.Errorf("log max size must be positive")
	}
	// maxBackups 小于 0 没意义，这里把它当成 0。
	// 0 的意思是：轮转时不保留旧文件。
	if maxBackups < 0 {
		maxBackups = 0
	}
	// filepath.Dir(path) 取出目录部分。
	// os.MkdirAll 会递归创建目录；0o755 是 Linux 文件权限写法。
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	// os.OpenFile 打开文件。
	// O_CREATE：不存在就创建。
	// O_APPEND：从文件末尾继续写。
	// O_WRONLY：只写。
	// 0o644：文件权限，所有者可读写，其他人只读。
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	// Stat 读取文件信息，包括当前大小。
	info, err := file.Stat()
	if err != nil {
		// 如果 Stat 失败，前面打开的文件也要关掉，避免资源泄漏。
		// _ 表示忽略 Close 返回的错误，因为这里已经要返回 Stat 的错误了。
		_ = file.Close()
		return nil, fmt.Errorf("stat log file: %w", err)
	}
	// &rotatingFile{...} 表示创建结构体并返回它的指针。
	return &rotatingFile{
		path:       path,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
		file:       file,
		size:       info.Size(),
	}, nil
}

// Write 写入一段日志内容。
//
// 这个方法签名是 Go 标准接口 io.Writer 要求的格式：
//
//	Write(p []byte) (int, error)
//
// p []byte 表示要写入的一段字节。
// 返回的 int 表示实际写了多少字节，error 表示是否失败。
func (f *rotatingFile) Write(p []byte) (int, error) {
	// Lock 上锁，避免同时写入和轮转。
	f.mu.Lock()

	// defer 表示函数结束前一定执行 Unlock。
	// 这样即使中间 return，也不会忘记解锁。
	defer f.mu.Unlock()

	// file 为 nil 表示已经 Close 过，不能再写。
	if f.file == nil {
		return 0, fmt.Errorf("log file is closed")
	}
	// 如果写入这段内容后会超过上限，就先轮转日志文件。
	// f.size > 0 是为了避免第一条超大日志导致空文件不断轮转。
	if f.size > 0 && f.size+int64(len(p)) > f.maxBytes {
		if err := f.rotate(); err != nil {
			return 0, err
		}
	}
	// 真正写入当前文件。
	n, err := f.file.Write(p)

	// n 是实际写入字节数，用它更新 size。
	f.size += int64(n)
	return n, err
}

// Close 关闭当前日志文件。
//
// 关闭后再调用 Write 会返回错误。
func (f *rotatingFile) Close() error {
	// 关闭文件也要上锁，避免另一个 goroutine 正在写。
	f.mu.Lock()
	defer f.mu.Unlock()

	// nil 表示已经关过了；重复 Close 直接当作成功。
	if f.file == nil {
		return nil
	}
	err := f.file.Close()

	// 关闭后把 file 置空，避免后续误用旧文件句柄。
	f.file = nil
	return err
}

// rotate 执行一次日志轮转。
//
// 轮转的效果：
//   - 当前日志 path 变成 path.1。
//   - 原来的 path.1 变成 path.2。
//   - 以此类推，最旧的超过 maxBackups 后被删除。
//   - 最后重新创建一个新的 path 文件继续写。
func (f *rotatingFile) rotate() error {
	// 先关闭当前文件，否则某些系统上重命名打开中的文件会有问题。
	if err := f.file.Close(); err != nil {
		return fmt.Errorf("close log before rotation: %w", err)
	}
	f.file = nil

	// maxBackups 为 0：不保存历史日志，直接删除当前日志文件。
	if f.maxBackups == 0 {
		if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove old log: %w", err)
		}
	} else {
		// 先删除最旧的备份，例如 bifrost.log.5。
		oldest := fmt.Sprintf("%s.%d", f.path, f.maxBackups)
		if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove oldest log backup: %w", err)
		}
		// 从后往前改名，避免覆盖。
		// 例如先把 .4 改成 .5，再把 .3 改成 .4。
		for i := f.maxBackups - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", f.path, i)
			dst := fmt.Sprintf("%s.%d", f.path, i+1)
			if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("rotate log backup %s: %w", src, err)
			}
		}
		// 当前日志改成 .1。
		if err := os.Rename(f.path, f.path+".1"); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotate current log: %w", err)
		}
	}

	// 创建新的当前日志文件。
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open new log file: %w", err)
	}
	f.file = file

	// 新文件大小从 0 开始。
	f.size = 0
	return nil
}

// parseByteSize 把 "10MB"、"512KB"、"100" 这样的字符串转成字节数。
//
// 返回 int64 是因为文件大小可能比较大。
func parseByteSize(value string) (int64, error) {
	// TrimSpace 去掉前后空格，避免 " 10MB " 解析失败。
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("byte size is empty")
	}

	// ToUpper 统一成大写，这样 mb、MB、Mb 都能识别。
	upper := strings.ToUpper(trimmed)

	// 这里用了“匿名 struct 切片”。
	// []struct{...} 表示一个列表，列表里的每一项都有 suffix 和 value 两个字段。
	multipliers := []struct {
		suffix string
		value  int64
	}{
		{suffix: "GB", value: 1024 * 1024 * 1024},
		{suffix: "G", value: 1024 * 1024 * 1024},
		{suffix: "MB", value: 1024 * 1024},
		{suffix: "M", value: 1024 * 1024},
		{suffix: "KB", value: 1024},
		{suffix: "K", value: 1024},
		{suffix: "B", value: 1},
	}
	// range 遍历每一种单位。
	// _ 常用于忽略不需要的下标；这里我们只需要 multiplier。
	for _, multiplier := range multipliers {
		// HasSuffix 判断字符串是否以某个单位结尾。
		if strings.HasSuffix(upper, multiplier.suffix) {
			// 去掉单位后，剩下的应该是数字部分。
			number := strings.TrimSpace(trimmed[:len(trimmed)-len(multiplier.suffix)])
			parsed, err := parsePositiveInt64(number)
			if err != nil {
				return 0, err
			}
			// 数字乘以单位倍率，得到字节数。
			return parsed * multiplier.value, nil
		}
	}
	// 如果没有单位，就把它当成纯字节数。
	return parsePositiveInt64(trimmed)
}

// parsePositiveInt 把字符串转成正整数 int。
//
// int 是 Go 里常用的整数类型，适合数量、下标这类值。
func parsePositiveInt(value string) (int, error) {
	// strconv.Atoi 是 ASCII to integer 的缩写，意思是把字符串转整数。
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, err
	}
	// 这里只接受大于 0 的数。
	if parsed <= 0 {
		return 0, fmt.Errorf("value must be positive")
	}
	return parsed, nil
}

// parsePositiveInt64 把字符串转成正整数 int64。
//
// int64 适合文件大小、时间戳这类可能更大的整数。
func parsePositiveInt64(value string) (int64, error) {
	// ParseInt 的参数含义：
	//   - value：要解析的字符串。
	//   - 10：十进制。
	//   - 64：结果使用 64 位整数。
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, err
	}
	// 这里只接受大于 0 的数。
	if parsed <= 0 {
		return 0, fmt.Errorf("value must be positive")
	}
	return parsed, nil
}
