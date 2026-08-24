package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

var (
	defaultCodexDatabase = filepath.Join(homeDir(), ".codex", "thread_history_1.sqlite")
	defaultClaudeDir     = filepath.Join(homeDir(), ".claude")
	defaultQoderDir      = filepath.Join(homeDir(), ".qoder-cn")
	defaultQoderAppDir   = filepath.Join(homeDir(), ".qoder-cn", "cache", "projects")
)

// options 保存命令行参数。
type options struct {
	session       string
	query         string
	turn          int
	think         bool
	archived      bool
	source        string
	dateText      string
	codexDatabase string
	claudeDir     string
	qoderDir      string
	qoderAppDir   string
}

func parseFlags() (*options, error) {
	opts := &options{}
	flag.StringVar(&opts.session, "session", "", "会话 ID，支持唯一前缀或 JSONL 文件路径")
	flag.StringVar(&opts.session, "i", "", "")
	flag.StringVar(&opts.query, "query", "", "按请求或最终响应文本过滤会话，不区分大小写")
	flag.StringVar(&opts.query, "q", "", "")
	flag.IntVar(&opts.turn, "turn", 0, "配合 --session 使用，输出指定问题序号的完整详情：问题、工具调用输入输出和回答")
	flag.IntVar(&opts.turn, "t", 0, "")
	flag.BoolVar(&opts.think, "think", false, "配合 --session 使用，输出会话中的中间思考过程")
	flag.BoolVar(&opts.archived, "archived", false, "列出 Codex 会话时包含已归档会话")
	flag.StringVar(&opts.source, "source", sourceAll,
		fmt.Sprintf("会话来源：%s/%s，all 表示全部", strings.Join(knownSources(), "/"), sourceAll))
	flag.StringVar(&opts.source, "s", sourceAll, "")
	flag.StringVar(&opts.dateText, "date", "", "按 TZ 时区过滤日期：YYYY-MM-DD、yesterday 或 all（全部日期），默认今天")
	flag.StringVar(&opts.dateText, "d", "", "")
	flag.StringVar(&opts.codexDatabase, "codex-db", defaultCodexDatabase, "Codex 历史数据库")
	flag.StringVar(&opts.claudeDir, "claude-dir", defaultClaudeDir, "Claude 数据目录")
	flag.StringVar(&opts.qoderDir, "qoder-dir", defaultQoderDir, "Qoder 数据目录")
	flag.StringVar(&opts.qoderAppDir, "qoder-app-dir", defaultQoderAppDir, "新版 Qoder 应用会话目录")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "解析 Codex、Claude、Qoder 或新版 Qoder 应用会话历史，输出输入、工具调用和最终输出。\n")
		fmt.Fprintf(out, "示例：ai-sessions -q tantivy；ai-sessions -i 019... -t 2；ai-sessions --source claude\n")
		aliases := map[string]string{"session": "i", "query": "q", "source": "s", "date": "d", "turn": "t"}
		flag.CommandLine.VisitAll(func(f *flag.Flag) {
			if f.Usage == "" {
				return
			}
			names := "--" + f.Name
			if alias, ok := aliases[f.Name]; ok {
				names = "-" + alias + ", --" + f.Name
			}
			typeName := strings.TrimSuffix(reflect.TypeOf(f.Value).Elem().Name(), "Value")
			if typeName != "bool" {
				names += " " + typeName
			}
			fmt.Fprintf(out, "  %s\n    \t%s", names, f.Usage)
			if typeName != "bool" && f.DefValue != "" && f.DefValue != "0" {
				fmt.Fprintf(out, " (default %s)", f.DefValue)
			}
			fmt.Fprintln(out)
		})
	}
	flag.Parse()
	if flag.NArg() > 0 {
		return nil, fmt.Errorf("不再支持位置参数，请改用 --session/-i 和 --query/-q：%s", strings.Join(flag.Args(), " "))
	}
	return opts, nil
}

// getTargetDate 解析日期参数，返回按 TZ 过滤的目标日期，支持 yesterday 和 all。
func getTargetDate(opts *options, loc *time.Location) (*time.Time, error) {
	if opts.dateText == "" {
		today := getToday(loc)
		return &today, nil
	}
	if opts.dateText == "yesterday" {
		yesterday := getToday(loc).AddDate(0, 0, -1)
		return &yesterday, nil
	}
	if opts.dateText == "all" {
		return nil, nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", opts.dateText, loc)
	if err != nil {
		return nil, fmt.Errorf("日期必须是 YYYY-MM-DD、yesterday 或 all：%s", opts.dateText)
	}
	return &parsed, nil
}

// validate 校验参数组合是否合法。
func (o *options) validate() error {
	if o.source != sourceAll && !isValidSource(o.source) {
		return fmt.Errorf("无效的 --source 值：%s（可选：%s/%s）", o.source, strings.Join(knownSources(), "/"), sourceAll)
	}
	if o.turn < 0 {
		return fmt.Errorf("问题序号必须大于 0：%d", o.turn)
	}
	if o.turn > 0 && o.session == "" {
		return errors.New("--turn 必须配合 --session 使用")
	}
	if o.turn > 0 && o.query != "" {
		return errors.New("不能同时使用 --turn 和 --query")
	}
	if o.think && o.session == "" {
		return errors.New("--think 必须配合 --session 使用")
	}
	return nil
}

func main() {
	opts, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(1)
	}
	if err := opts.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(1)
	}

	loc, err := getDisplayTimezone()
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(1)
	}
	targetDate, err := getTargetDate(opts, loc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(1)
	}

	if opts.session != "" {
		turnNumber := opts.turn
		source := opts.source
		if source == "all" {
			source = detectSource(opts.session, opts.claudeDir, opts.qoderDir, opts.qoderAppDir)
		}
		captureToolDetails := turnNumber > 0
		captureThinking := opts.think
		var session *SessionData
		switch source {
		case sourceCodex:
			session, err = parseCodex(opts.session, opts.codexDatabase, loc, captureToolDetails, captureThinking)
		case sourceQoder:
			session, err = parseJSONLBySource(sourceQoder, opts.session, opts.qoderDir, loc, false, captureToolDetails, captureThinking)
		case sourceQoderApp:
			session, err = parseQoderApp(opts.session, opts.qoderAppDir, loc, captureThinking)
		default:
			session, err = parseJSONLBySource(sourceClaude, opts.session, opts.claudeDir, loc, false, captureToolDetails, captureThinking)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			os.Exit(1)
		}
		if turnNumber > 0 {
			if turnNumber > len(session.Turns) {
				fmt.Fprintf(os.Stderr, "错误：问题序号超出范围：%d（会话共 %d 轮）\n", turnNumber, len(session.Turns))
				os.Exit(1)
			}
			printTurnDetail(session, turnNumber, loc, useTerminalColor(), opts.think)
			return
		}
		if !session.matchesQuery(opts.query) {
			fmt.Fprintf(os.Stderr, "错误：会话不匹配查询：%s\n", opts.query)
			os.Exit(1)
		}
		printSession(session, loc, useTerminalColor(), true, opts.think)
		return
	}

	sessions, err := loadAllSessions(opts.source, opts.codexDatabase, opts.claudeDir, opts.qoderDir, opts.qoderAppDir, loc, targetDate, opts.archived)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(1)
	}
	if opts.query != "" {
		printMatchingSessions(sessions, opts.query, loc, targetDate)
	} else {
		printSessionIndex(sessions, loc)
	}
}
