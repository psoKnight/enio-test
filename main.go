package main

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/mark3labs/mcp-go/client/transport"
	"log"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einomcp "github.com/cloudwego/eino-ext/components/tool/mcp"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	_ "github.com/go-sql-driver/mysql"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

type Intent string

const (
	QueryData Intent = "query_data"
	Explain   Intent = "explain"
	Other     Intent = "other"
)

type SessionInput struct {
	SessionID string
	Query     string
}

type GraphState struct {
	SessionID      string
	CurrentQuery   string
	RewrittenQuery string
	History        []*schema.Message
	Intent         Intent
	Response       string
}

// =====================
// MySQL 会话存储层
// =====================
type MySQLSessionStore struct {
	db *sql.DB
}

func NewMySQLSessionStore(dsn string) (*MySQLSessionStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("连接 MySQL 失败: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("MySQL Ping 失败: %w", err)
	}
	return &MySQLSessionStore{db: db}, nil
}

func (s *MySQLSessionStore) LoadHistory(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	//rows, err := s.db.QueryContext(ctx, "SELECT role, content FROM session_memory WHERE session_id = ? ORDER BY idx ASC", sessionID)
	rows, err := s.db.QueryContext(ctx, "SELECT role, content FROM session_memory WHERE session_id = ? and role = ? ORDER BY idx ASC limit 3", sessionID, schema.User)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []*schema.Message
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return nil, err
		}
		switch strings.ToLower(role) {
		case string(schema.User):
			history = append(history, schema.UserMessage(content))
		case string(schema.Assistant):
			history = append(history, schema.AssistantMessage(content, nil))
		}
	}
	return history, rows.Err()
}

func (s *MySQLSessionStore) SaveTurn(ctx context.Context, sessionID string, userMsg, assistantMsg *schema.Message) error {
	var maxIdx sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(idx) FROM session_memory WHERE session_id = ?", sessionID).Scan(&maxIdx)
	if err != nil {
		return err
	}
	nextIdx := int(maxIdx.Int64) + 1

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO session_memory (session_id, role, content, idx) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	if _, err := stmt.ExecContext(ctx, sessionID, string(schema.User), userMsg.Content, nextIdx); err != nil {
		return err
	}
	if _, err := stmt.ExecContext(ctx, sessionID, string(schema.Assistant), assistantMsg.Content, nextIdx+1); err != nil {
		return err
	}

	return tx.Commit()
}

func main() {
	ctx := context.Background()

	// 1. 初始化 LLM & MCP & Agent
	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  "xxx",
		Model:   "Doubao-1.5-pro-32k",
		BaseURL: "https://llm.api.zyuncs.com/v1",
	})
	if err != nil {
		log.Fatal("初始化 LLM 失败:", err)
	}

	c, err := client.NewStreamableHttpClient("http://p38222v.hulk.bjwdt.qihoo.net:9530/mcp", transport.WithHTTPTimeout(time.Duration(1)*time.Minute))
	if err != nil {
		log.Fatal("创建 MCP Client 失败:", err)
	}
	_, err = c.Initialize(ctx, mcp.InitializeRequest{})
	if err != nil {
		log.Fatal("MCP 初始化失败:", err)
	}

	tools, err := einomcp.GetTools(ctx, &einomcp.Config{Cli: c})
	if err != nil {
		log.Fatal("获取 MCP Tools 失败:", err)
	}

	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Model: llm,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools},
		},
	})
	if err != nil {
		log.Fatal("创建 Agent 失败:", err)
	}
	agentRunner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent})

	// 2. 初始化 MySQL
	store, err := NewMySQLSessionStore("sparkbigdata_wr:3cc0d6e74c4e2a06@tcp(rw-7b81d0.bjmd.mysql.qihudb.net:22088)/sparkbigdata?charset=utf8mb4&parseTime=True&loc=Local")
	defer store.db.Close()

	// =====================
	// 3. 定义图节点
	// =====================
	initNode := compose.InvokableLambda[*SessionInput, *GraphState](func(ctx context.Context, in *SessionInput) (*GraphState, error) {
		return &GraphState{SessionID: in.SessionID, CurrentQuery: in.Query}, nil
	})

	loadHistNode := compose.InvokableLambda[*GraphState, *GraphState](func(ctx context.Context, s *GraphState) (*GraphState, error) {
		hist, err := store.LoadHistory(ctx, s.SessionID)
		if err != nil {
			log.Printf("⚠️ 加载历史失败: %v", err)
			return s, nil
		}
		s.History = hist
		return s, nil
	})

	rewriteNode := compose.InvokableLambda[*GraphState, *GraphState](func(ctx context.Context, s *GraphState) (*GraphState, error) {
		if len(s.History) == 0 {
			s.RewrittenQuery = s.CurrentQuery
			return s, nil
		}

		sysPrompt := `你是一个多轮对话上下文补全专家。
【输入格式】
你将接收一个字符串数组。数组按时间顺序排列，【最后一条元素】是用户当前问题，前面的元素是对话历史。

【核心原则】
1. 最新提问绝对优先：必须保留最后一条问题中的所有实体/指标/时间。历史信息仅用于填补最后一条问题的【缺失部分】。
2. 冲突零容忍：若最后一条问题已包含某维度，历史同类信息必须忽略，严禁替换或覆盖。
3. 意图隔离：若用户切换查询主体/指标，以最后一条问题为准，绝不强行融合历史意图。
4. 完整即原样：若最后一条问题已语义完整、无指代歧义，必须原样输出。
5. 精准消解：仅当最后一条问题含指代词（它/这/那/呢）或缺失关键维度时，才从历史提取信息补全。

【输出规范】
- 仅输出改写后的单句问题
- 禁止任何解释、前缀、后缀、引号、换行
- 保持自然查询语序

【Few-Shot 示例】
输入：["查询清理大师最近3天的花费。", "那点击率呢"] → 输出：查询清理大师最近3天的点击率。
输入：["查询清理大师最近7天的数据。", "什么是ROI？"] → 输出：请解释ROI 指标。
输入：["查询清理大师最近3天的花费。", "什么是ROI？", "请计算手心输入的法oppo 媒体的花费。"] → 输出： 请计算手心输入的法oppo 媒体最近3天的花费。

请处理以下输入：`

		msgs := []*schema.Message{schema.SystemMessage(sysPrompt)}
		msgs = append(msgs, s.History...)
		msgs = append(msgs, schema.UserMessage(s.CurrentQuery))

		fmt.Println("-----------------")
		fmt.Println("rewriteNode 输入：")
		fmt.Println(msgs)
		fmt.Println("-----------------")

		resp, err := llm.Generate(ctx, msgs)
		if err != nil || strings.TrimSpace(resp.Content) == "" {
			s.RewrittenQuery = s.CurrentQuery
			return s, nil
		}
		s.RewrittenQuery = strings.TrimSpace(resp.Content)

		fmt.Println("rewriteNode RewrittenQuery: " + s.RewrittenQuery)

		return s, nil
	})

	intentNode := compose.InvokableLambda[*GraphState, *GraphState](func(ctx context.Context, s *GraphState) (*GraphState, error) {
		query := s.RewrittenQuery
		sysin := `你是一个投放业务助手中的意图分类器，负责理解用户在广告投放场景下的需求并进行分类。

分类规则：
- query_data：查询投放相关业务数据或指标（如花费、消耗、转化、ROI、计划/广告/创意表现等，含任何时间或筛选条件）
- explain：解释投放领域的术语、概念、规则（如oCPM、CTR、转化归因等）
- other：闲聊、问候、无明确投放意图的问题

注意：
- 只要涉及广告投放数据、报表、分析、筛选，都归为query_data
- 只要在解释投放行业知识，都归为explain
- 不要输出任何解释或多余内容
- 仅返回一个词：query_data / explain / other

请处理以下输入：`
		msgs := []*schema.Message{schema.SystemMessage(sysin)}
		//msgs = append(msgs, s.History...)
		msgs = append(msgs, schema.UserMessage(query))
		resp, err := llm.Generate(ctx, msgs)
		if err != nil {
			s.Intent = Other
			return s, nil
		}
		out := strings.ToLower(strings.TrimSpace(resp.Content))
		switch {
		case strings.Contains(out, "query_data"):
			s.Intent = QueryData
		case strings.Contains(out, "explain"):
			s.Intent = Explain
		default:
			s.Intent = Other
		}

		fmt.Println("识别意图： ", s.Intent)

		return s, nil
	})

	// ✅ Agent 子流（安全流式）
	agentChain := compose.NewChain[*GraphState, *GraphState]()
	_ = agentChain.AppendLambda(compose.InvokableLambda[*GraphState, *GraphState](func(ctx context.Context, s *GraphState) (*GraphState, error) {

		start := time.Now() // 开始时间

		// TODO 此处可以尝试追加历史问答

		// 当前 Query
		msgs := make([]*schema.Message, 0)
		msgs = append(msgs, schema.SystemMessage(fmt.Sprintf("你是一个广告投放数据查询助手，只负责一件事：调用MCP 工具查询数据。如果用户没有要求格式，输出结果需要使用表格显示（注意表格的设计，比如必须使用表头），保证数据展示清晰直观。")))

		if s.RewrittenQuery != "" {
			msgs = append(msgs, schema.UserMessage(s.RewrittenQuery))
		} else {
			msgs = append(msgs, schema.UserMessage(s.CurrentQuery))
		}

		fmt.Println("-----------------")
		fmt.Println("agentChain 输入：")
		fmt.Println(msgs)
		fmt.Println("-----------------")

		// 调用 Agent Runner (Eino v0.8.11 支持传入 []Message)
		iter := agentRunner.Run(ctx, msgs)

		var fullContent strings.Builder
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}

			if event != nil && event.Output != nil && event.Output.MessageOutput != nil && event.Output.MessageOutput.Message != nil {
				fullContent.WriteString(event.Output.MessageOutput.Message.Content)
			}
		}

		s.Response = strings.TrimSpace(fullContent.String())
		if s.Response == "" {
			s.Response = "[模型未返回有效内容]"
		}

		duration := time.Since(start) // 计算耗时
		fmt.Printf("agentChain 执行时间: %v\n\n", duration)
		return s, nil
	}))

	// 🔧 修复：KB 节点改为就地修改 state，禁止返回新 struct 导致字段丢失
	kbNode := compose.InvokableLambda[*GraphState, *GraphState](func(ctx context.Context, s *GraphState) (*GraphState, error) {
		msgs := append(s.History, schema.UserMessage(s.CurrentQuery))
		resp, err := llm.Generate(ctx, msgs)
		if err != nil {
			s.Response = "知识库调用失败: " + err.Error()
			return s, nil
		}
		content := strings.TrimSpace(resp.Content)
		if content == "" {
			s.Response = "[模型未返回有效内容]"
		} else {
			s.Response = content
		}
		return s, nil
	})

	// 🔧 修复：兜底节点就地修改 state
	fallbackNode := compose.InvokableLambda[*GraphState, *GraphState](func(ctx context.Context, s *GraphState) (*GraphState, error) {
		s.Response = "无法回答该问题，请尝试使用更明确的指令。"
		return s, nil
	})

	// 🔧 修复：移除 Intent==Other 拦截，统一全意图落库
	saveHistNode := compose.InvokableLambda[*GraphState, *GraphState](func(ctx context.Context, s *GraphState) (*GraphState, error) {
		cleanResp := strings.TrimSpace(s.Response)
		if cleanResp == "" || strings.Contains(cleanResp, "[模型未返回有效内容]") || strings.Contains(cleanResp, "[Agent调用失败]") {
			log.Printf("⚠️ 拦截空/异常响应，跳过落库。SessionID: %s, Intent: %s", s.SessionID, s.Intent)
			return s, nil
		}
		cleanQuery := strings.TrimSpace(s.CurrentQuery)
		if cleanQuery == "" {
			return s, nil
		}

		if err := store.SaveTurn(ctx, s.SessionID, schema.UserMessage(cleanQuery), schema.AssistantMessage(cleanResp, nil)); err != nil {
			log.Printf("⚠️ 保存历史失败: %v", err)
			return s, nil
		}
		log.Printf("✅ 落库成功 | Session: %s | Intent: %s | Query: %s", s.SessionID, s.Intent, cleanQuery)
		s.History = append(s.History, schema.UserMessage(cleanQuery), schema.AssistantMessage(cleanResp, nil))
		return s, nil
	})

	extractNode := compose.InvokableLambda[*GraphState, string](func(ctx context.Context, s *GraphState) (string, error) {
		return s.Response, nil
	})

	// =====================
	// 4. 构建图
	// =====================
	g := compose.NewGraph[*SessionInput, string]()
	_ = g.AddLambdaNode("init", initNode)
	_ = g.AddLambdaNode("load_hist", loadHistNode)
	_ = g.AddLambdaNode("rewrite", rewriteNode)
	_ = g.AddLambdaNode("classify", intentNode)
	_ = g.AddGraphNode("agent_flow", agentChain)
	_ = g.AddLambdaNode("kb", kbNode)
	_ = g.AddLambdaNode("fallback", fallbackNode)
	_ = g.AddLambdaNode("save_hist", saveHistNode)
	_ = g.AddLambdaNode("extract", extractNode)

	_ = g.AddEdge(compose.START, "init")
	_ = g.AddEdge("init", "load_hist")
	_ = g.AddEdge("load_hist", "rewrite")
	_ = g.AddEdge("rewrite", "classify")

	branch := compose.NewGraphBranch(func(ctx context.Context, state *GraphState) (string, error) {
		switch state.Intent {
		case QueryData:
			return "agent_flow", nil
		case Explain:
			return "kb", nil
		default:
			return "fallback", nil
		}
	}, map[string]bool{"agent_flow": true, "kb": true, "fallback": true})
	_ = g.AddBranch("classify", branch)

	_ = g.AddEdge("agent_flow", "save_hist")
	_ = g.AddEdge("kb", "save_hist")
	_ = g.AddEdge("fallback", "save_hist")
	_ = g.AddEdge("save_hist", "extract")
	_ = g.AddEdge("extract", compose.END)

	runner, err := g.Compile(ctx)
	if err != nil {
		log.Fatal("图编译失败:", err)
	}

	sessionID := "sess_all_intents"
	examples := []string{
		"请帮我查询清理大师最近3天的花费数据。", // 查询数据
		"手心输入法oppo 媒体的花费呢？",   // 查询数据
		"CTR 计算公式是？",          // 名词解释
		"1+1=？",               // 其他
	}

	fmt.Println("=== 全意图存储测试 ===")
	for _, q := range examples {
		fmt.Printf("👤 用户: %s\n", q)

		start := time.Now() // 开始时间

		out, err := runner.Invoke(ctx, &SessionInput{
			SessionID: sessionID,
			Query:     q,
		})

		duration := time.Since(start) // 计算耗时

		if err != nil {
			log.Printf("❌ 失败: %v | ⏱️ 耗时: %v\n", err, duration)
			continue
		}

		fmt.Printf("🤖 AI:   %s\n", out)
		fmt.Printf("⏱️ 执行时间: %v\n\n", duration)
	}
}
