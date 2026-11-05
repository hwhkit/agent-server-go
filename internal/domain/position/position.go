// Package position 定义岗位：一份工作的边界和能力范畴。
//
// 岗位是职责定义（能力边界、角色契约、输出 schema），不含任何执行配置；
// 执行配置属于智能体（domain/agent）。内置六岗位的角色契约正文由
// golden 快照守护（迁移自旧包的 TestContractsGolden）。
package position

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 内置岗位 Code，与角色契约 golden 快照键一一对应。
const (
	CodeFundamentalAnalyst = "FUNDAMENTAL_ANALYST"
	CodeTechnicalAnalyst   = "TECHNICAL_ANALYST"
	CodeStrategist         = "STRATEGIST"
	CodeRiskManager        = "RISK_MANAGER"
	CodeExecutionTrader    = "EXECUTION_TRADER"
	CodePortfolioManager   = "PORTFOLIO_MANAGER"
)

// CodeCustom 是迁移兜底岗位：seed 时无 position_type 的存量智能体聘任到
// 此岗位。不允许经管理 API 手工创建或删除。
const CodeCustom = "__CUSTOM__"

// BuiltinCodes 是六个内置岗位，顺序即控制台展示顺序。
var BuiltinCodes = []string{
	CodeFundamentalAnalyst,
	CodeTechnicalAnalyst,
	CodeStrategist,
	CodeRiskManager,
	CodeExecutionTrader,
	CodePortfolioManager,
}

// IsBuiltin 报告 Code 是否为内置岗位。
func IsBuiltin(code string) bool {
	for _, c := range BuiltinCodes {
		if c == code {
			return true
		}
	}
	return false
}

// Position 是岗位实体。
type Position struct {
	Code        string
	Name        string
	Description string
	// ContractRef 引用角色契约文本：内置岗位固定为岗位 Code；自定义岗位
	// 为 "custom:<id>"。正文经 ContractTextProvider 获取。
	ContractRef  string
	OutputSchema json.RawMessage // 可空；输出契约 JSON Schema
	Enabled      bool
	Revision     int64
}

var codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)

// ErrNotFound 是仓储未命中哨兵。
var ErrNotFound = errors.New("position not found")

// ErrInUse 表示岗位仍被智能体聘任，不可删除。
var ErrInUse = errors.New("position is still staffed by agents")

// ErrReserved 表示对保留岗位（__CUSTOM__）的非法操作。
var ErrReserved = errors.New("reserved position")

// ApplyDefaults 填充缺省值。
func (p *Position) ApplyDefaults() {
	p.Code = strings.ToUpper(strings.TrimSpace(p.Code))
	p.Name = strings.TrimSpace(p.Name)
	p.Description = strings.TrimSpace(p.Description)
	p.ContractRef = strings.TrimSpace(p.ContractRef)
	if p.ContractRef == "" {
		p.ContractRef = p.Code
	}
}

// Validate 规范化并校验岗位。
func (p *Position) Validate() error {
	p.ApplyDefaults()
	if p.Code == CodeCustom {
		return fmt.Errorf("%w: %s cannot be created via API", ErrReserved, CodeCustom)
	}
	if !codePattern.MatchString(p.Code) {
		return errors.New("code must match ^[A-Z][A-Z0-9_]{1,63}$")
	}
	if n := utf8.RuneCountInString(p.Name); n < 1 || n > 60 {
		return errors.New("name must be 1-60 characters")
	}
	if IsBuiltin(p.Code) && p.ContractRef != p.Code {
		// 内置岗位的契约就是岗位身份的一部分，不允许改指。
		return errors.New("builtin position contract ref is fixed to its code")
	}
	if !IsBuiltin(p.Code) && !strings.HasPrefix(p.ContractRef, "custom:") {
		return errors.New(`custom position contract ref must use the "custom:<id>" form`)
	}
	if len(p.OutputSchema) > 0 && !json.Valid(p.OutputSchema) {
		return errors.New("output_schema must be valid JSON")
	}
	return nil
}

// Repository 是岗位仓储端口。Upsert 在事务中递增 revision；
// ifMatchRevision 非空时不匹配返回 ErrRevisionConflict（乐观并发）。
type Repository interface {
	GetByCode(ctx context.Context, code string) (*Position, error)
	List(ctx context.Context, includeDisabled bool) ([]Position, error)
	Upsert(ctx context.Context, p *Position, ifMatchRevision *int64) (int64, error)
	SetEnabled(ctx context.Context, code string, enabled bool, ifMatchRevision *int64) (int64, error)
	// Delete 在岗位仍被聘任时必须返回 ErrInUse。
	Delete(ctx context.Context, code string) error
}

// ErrRevisionConflict 是 If-Match 乐观并发冲突哨兵。
var ErrRevisionConflict = errors.New("revision conflict")

// ContractTextProvider 按引用提供角色契约正文。内置契约由 golden 快照
// 守护；未命中返回 ErrNotFound。
type ContractTextProvider interface {
	ContractText(ctx context.Context, ref string) (string, error)
}
