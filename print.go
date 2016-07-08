// Copyright (c) 2016, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package sh

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"
)

// PrintConfig controls how the printing of an AST node will behave.
type PrintConfig struct {
	Spaces int // 0 (default) for tabs, >0 for number of spaces
}

var writerFree = sync.Pool{
	New: func() interface{} { return bufio.NewWriter(nil) },
}

// Fprint "pretty-prints" the given AST file to the given writer.
func (c PrintConfig) Fprint(w io.Writer, f *File) error {
	bw := writerFree.Get().(*bufio.Writer)
	bw.Reset(w)
	p := printer{
		w: bw,
		f: f,
		c: c,
	}
	p.comments = f.Comments
	p.stmts(f.Stmts)
	p.commentsUpTo(0)
	p.newline()
	if p.err == nil {
		p.err = bw.Flush()
	}
	writerFree.Put(p.w)
	return p.err
}

// Fprint "pretty-prints" the given AST file to the given writer. It
// calls PrintConfig.Fprint with its default settings.
func Fprint(w io.Writer, f *File) error {
	return PrintConfig{}.Fprint(w, f)
}

type bufWriter interface {
	io.Writer
	WriteString(string) (int, error)
	WriteByte(byte) error
}

type printer struct {
	w   bufWriter
	f   *File
	c   PrintConfig
	err error

	nestedBinary bool

	wantSpace   bool
	wantNewline bool
	wantSpaces  int

	// curLine is the line that is currently being printed (counted
	// in original lines).
	curLine int
	// lastLevel is the last level of indentation that was used.
	lastLevel int
	// level is the current level of indentation.
	level int
	// levelIncs records which indentation level increments actually
	// took place, to revert them once their section ends.
	levelIncs []bool

	// comments is the list of pending comments to write.
	comments []*Comment

	// pendingHdocs is the list of pending heredocs to write.
	pendingHdocs []*Redirect
}

func (p *printer) space() {
	p.err = p.w.WriteByte(' ')
	p.wantSpace = false
}

func (p *printer) spaces(n int) {
	for i := 0; i < n; i++ {
		p.w.WriteByte(' ')
	}
	p.wantSpace = false
}

func (p *printer) tabs(n int) {
	for i := 0; i < n; i++ {
		p.w.WriteByte('\t')
	}
	p.wantSpace = false
}

func (p *printer) bslashNewl() {
	_, p.err = p.w.WriteString(" \\\n")
	p.wantSpace = false
	p.curLine++
}

func (p *printer) str(s string) {
	_, p.err = p.w.WriteString(s)
}

func (p *printer) byte(b byte) {
	p.err = p.w.WriteByte(b)
}

func (p *printer) token(s string, spaceAfter bool) {
	p.wantSpace = spaceAfter
	_, p.err = p.w.WriteString(s)
}

func (p *printer) rsrv(s string) {
	_, p.err = p.w.WriteString(s)
	p.wantSpace = true
}

func (p *printer) spacedRsrv(s string) {
	if p.wantSpace {
		p.space()
	}
	_, p.err = p.w.WriteString(s)
	p.wantSpace = true
}

func (p *printer) spacedTok(s string, spaceAfter bool) {
	if p.wantSpace {
		p.space()
	}
	p.wantSpace = spaceAfter
	_, p.err = p.w.WriteString(s)
}

func (p *printer) semiOrNewl(s string, pos Pos) {
	if p.wantNewline {
		p.newline()
		p.indent()
	} else {
		p.str("; ")
	}
	_, p.err = p.w.WriteString(s)
	p.wantSpace = true
	p.curLine = p.f.Position(pos).Line
}

func (p *printer) incLevel() {
	inc := false
	if p.level <= p.lastLevel {
		p.level++
		inc = true
	} else if last := &p.levelIncs[len(p.levelIncs)-1]; *last {
		*last = false
		inc = true
	}
	p.levelIncs = append(p.levelIncs, inc)
}

func (p *printer) decLevel() {
	inc := p.levelIncs[len(p.levelIncs)-1]
	p.levelIncs = p.levelIncs[:len(p.levelIncs)-1]
	if inc {
		p.level--
	}
}

func (p *printer) indent() {
	p.lastLevel = p.level
	switch {
	case p.level == 0:
	case p.c.Spaces == 0:
		p.tabs(p.level)
	case p.c.Spaces > 0:
		p.spaces(p.c.Spaces * p.level)
	}
}

func (p *printer) newline() {
	p.wantNewline = false
	p.err = p.w.WriteByte('\n')
	p.wantSpace = false
	for _, r := range p.pendingHdocs {
		p.str(r.Hdoc.Value)
		p.curLine += strings.Count(r.Hdoc.Value, "\n")
		p.unquotedWord(&r.Word)
		p.err = p.w.WriteByte('\n')
		p.curLine++
		p.wantSpace = false
	}
	p.pendingHdocs = nil
}

func (p *printer) newlines(pos Position) {
	p.newline()
	if pos.Line > p.curLine+1 {
		// preserve single empty lines
		p.err = p.w.WriteByte('\n')
	}
	p.indent()
	p.curLine = pos.Line
}

func (p *printer) alwaysSeparate(pos Position) {
	p.commentsUpTo(pos.Line)
	if p.curLine > 0 {
		p.newlines(pos)
	} else {
		p.curLine = pos.Line
	}
}

func (p *printer) didSeparate(pos Position) bool {
	p.commentsUpTo(pos.Line)
	if p.wantNewline || (p.curLine > 0 && pos.Line > p.curLine) {
		p.newlines(pos)
		return true
	}
	if p.curLine == 0 {
		p.curLine = pos.Line
		return true
	}
	p.curLine = pos.Line
	return false
}

func (p *printer) sepTok(s string, pos Position) {
	p.level++
	p.commentsUpTo(pos.Line)
	p.level--
	p.didSeparate(pos)
	if s != ")" && p.wantSpace {
		p.space()
	}
	_, p.err = p.w.WriteString(s)
	p.wantSpace = true
}

func (p *printer) semiRsrv(s string, rpos Pos, fallback bool) {
	pos := p.f.Position(rpos)
	p.level++
	p.commentsUpTo(pos.Line)
	p.level--
	if !p.didSeparate(pos) && fallback {
		p.str("; ")
	} else if p.wantSpace {
		p.space()
	}
	_, p.err = p.w.WriteString(s)
	p.wantSpace = true
}

func (p *printer) hasInline(pos Position) bool {
	if len(p.comments) < 1 {
		return false
	}
	for _, c := range p.comments {
		cpos := p.f.Position(c.Hash)
		if cpos.Line == pos.Line {
			return true
		}
		if cpos.Line > pos.Line {
			return false
		}
	}
	return false
}

func (p *printer) commentsUpTo(line int) {
	if len(p.comments) < 1 {
		return
	}
	c := p.comments[0]
	cpos := p.f.Position(c.Hash)
	if line > 0 && cpos.Line >= line {
		return
	}
	p.wantNewline = false
	if !p.didSeparate(cpos) {
		p.spaces(p.wantSpaces + 1)
	}
	p.err = p.w.WriteByte('#')
	_, p.err = p.w.WriteString(c.Text)
	p.comments = p.comments[1:]
	p.commentsUpTo(line)
}

func quotedOp(tok Token) string {
	switch tok {
	case DQUOTE:
		return `"`
	case DOLLSQ:
		return `$'`
	case SQUOTE:
		return `'`
	default: // DOLLDQ
		return `$"`
	}
}

func expansionOp(tok Token) string {
	switch tok {
	case COLON:
		return ":"
	case ADD:
		return "+"
	case CADD:
		return ":+"
	case SUB:
		return "-"
	case CSUB:
		return ":-"
	case QUEST:
		return "?"
	case CQUEST:
		return ":?"
	case ASSIGN:
		return "="
	case CASSIGN:
		return ":="
	case REM:
		return "%"
	case DREM:
		return "%%"
	case HASH:
		return "#"
	default: // DHASH
		return "##"
	}
}

func (p *printer) wordPart(wp WordPart) {
	switch x := wp.(type) {
	case *Lit:
		_, p.err = p.w.WriteString(x.Value)
	case *SglQuoted:
		p.byte('\'')
		_, p.err = p.w.WriteString(x.Value)
		p.curLine += strings.Count(x.Value, "\n")
		p.byte('\'')
	case *Quoted:
		p.str(quotedOp(x.Quote))
		for _, n := range x.Parts {
			p.wordPart(n)
			p.curLine = p.f.Position(n.End()).Line
		}
		p.str(quotedOp(quotedStop(x.Quote)))
	case *CmdSubst:
		p.wantSpace = false
		if x.Backquotes {
			p.byte('`')
		} else {
			p.str("$(")
		}
		if startsWithLparen(x.Stmts) {
			p.space()
		}
		p.nestedStmts(x.Stmts)
		if x.Backquotes {
			p.wantSpace = false
			p.sepTok("`", p.f.Position(x.Right))
		} else {
			p.sepTok(")", p.f.Position(x.Right))
		}
	case *ParamExp:
		if x.Short {
			p.byte('$')
			p.str(x.Param.Value)
			break
		}
		p.str("${")
		if x.Length {
			p.byte('#')
		}
		p.str(x.Param.Value)
		if x.Ind != nil {
			p.byte('[')
			p.word(x.Ind.Word)
			p.byte(']')
		}
		if x.Repl != nil {
			if x.Repl.All {
				p.byte('/')
			}
			p.byte('/')
			p.word(x.Repl.Orig)
			p.byte('/')
			p.word(x.Repl.With)
		} else if x.Exp != nil {
			p.str(expansionOp(x.Exp.Op))
			p.word(x.Exp.Word)
		}
		p.byte('}')
	case *ArithmExp:
		p.str("$((")
		p.arithm(x.X, false)
		p.str("))")
	case *ArrayExpr:
		p.wantSpace = false
		p.byte('(')
		p.wordJoin(x.List, false)
		p.sepTok(")", p.f.Position(x.Rparen))
	case *ProcSubst:
		// avoid conflict with << and others
		if p.wantSpace {
			p.space()
		}
		switch x.Op {
		case CMDIN:
			p.str("<(")
		case CMDOUT:
			p.str(">(")
		}
		p.nestedStmts(x.Stmts)
		p.byte(')')
	}
	p.wantSpace = true
}

func (p *printer) cond(cond Cond) {
	switch x := cond.(type) {
	case *StmtCond:
		p.nestedStmts(x.Stmts)
	case *CStyleCond:
		p.spacedTok("((", false)
		p.arithm(x.X, false)
		p.str("))")
	}
}

func (p *printer) loop(loop Loop) {
	switch x := loop.(type) {
	case *WordIter:
		p.str(x.Name.Value)
		if len(x.List) > 0 {
			p.rsrv(" in")
			p.wordJoin(x.List, true)
		}
	case *CStyleLoop:
		p.str("((")
		p.arithm(x.Init, false)
		p.str("; ")
		p.arithm(x.Cond, false)
		p.str("; ")
		p.arithm(x.Post, false)
		p.str("))")
	}
}

func binaryExprOp(tok Token) string {
	switch tok {
	case ASSIGN:
		return "="
	case ADD:
		return "+"
	case SUB:
		return "-"
	case REM:
		return "%"
	case MUL:
		return "*"
	case QUO:
		return "/"
	case AND:
		return "&"
	case OR:
		return "|"
	case LAND:
		return "&&"
	case LOR:
		return "||"
	case XOR:
		return "^"
	case POW:
		return "**"
	case EQL:
		return "=="
	case NEQ:
		return "!="
	case LEQ:
		return "<="
	case GEQ:
		return ">="
	case ADDASSGN:
		return "+="
	case SUBASSGN:
		return "-="
	case MULASSGN:
		return "*="
	case QUOASSGN:
		return "/="
	case REMASSGN:
		return "%="
	case ANDASSGN:
		return "&="
	case ORASSGN:
		return "|="
	case XORASSGN:
		return "^="
	case SHLASSGN:
		return "<<="
	case SHRASSGN:
		return ">>="
	case LSS:
		return "<"
	case GTR:
		return ">"
	case SHL:
		return "<<"
	case SHR:
		return ">>"
	case QUEST:
		return "?"
	case COLON:
		return ":"
	default: // COMMA
		return ","
	}
}

func unaryExprOp(tok Token) string {
	switch tok {
	case ADD:
		return "+"
	case SUB:
		return "-"
	case NOT:
		return "!"
	case INC:
		return "++"
	default: // DEC
		return "--"
	}
}
func (p *printer) arithm(expr ArithmExpr, compact bool) {
	p.wantSpace = false
	switch x := expr.(type) {
	case *Word:
		p.spacedWord(*x)
	case *BinaryExpr:
		if compact {
			p.arithm(x.X, true)
			p.str(binaryExprOp(x.Op))
			p.arithm(x.Y, true)
		} else {
			p.arithm(x.X, false)
			if x.Op != COMMA {
				p.space()
			}
			p.str(binaryExprOp(x.Op))
			p.space()
			p.arithm(x.Y, false)
		}
	case *UnaryExpr:
		if x.Post {
			p.arithm(x.X, compact)
			p.str(unaryExprOp(x.Op))
		} else {
			p.str(unaryExprOp(x.Op))
			p.arithm(x.X, compact)
		}
	case *ParenExpr:
		p.byte('(')
		p.arithm(x.X, false)
		p.byte(')')
	}
}

func (p *printer) word(w Word) {
	for _, n := range w.Parts {
		p.wordPart(n)
	}
}

func (p *printer) unquotedWord(w *Word) {
	for _, wp := range w.Parts {
		switch x := wp.(type) {
		case *SglQuoted:
			p.str(x.Value)
		case *Quoted:
			for _, qp := range x.Parts {
				p.wordPart(qp)
			}
		case *Lit:
			if x.Value[0] == '\\' {
				p.str(x.Value[1:])
			} else {
				p.str(x.Value)
			}
		default:
			p.wordPart(wp)
		}
	}
}

func (p *printer) spacedWord(w Word) {
	if p.wantSpace {
		p.space()
	}
	for _, n := range w.Parts {
		p.wordPart(n)
	}
}

func (p *printer) wordJoin(ws []Word, needBackslash bool) {
	anyNewline := false
	for _, w := range ws {
		if p.curLine > 0 && p.f.Position(w.Pos()).Line > p.curLine {
			if needBackslash {
				p.bslashNewl()
			} else {
				p.err = p.w.WriteByte('\n')
				p.curLine++
			}
			if !anyNewline {
				p.incLevel()
				anyNewline = true
			}
			p.indent()
		} else if p.wantSpace {
			p.space()
		}
		for _, n := range w.Parts {
			p.wordPart(n)
		}
	}
	if anyNewline {
		p.decLevel()
	}
}

func (p *printer) stmt(s *Stmt) {
	if s.Negated {
		p.spacedRsrv("!")
	}
	p.assigns(s.Assigns)
	startRedirs := p.command(s.Cmd, s.Redirs)
	anyNewline := false
	for _, r := range s.Redirs[startRedirs:] {
		pos := p.f.Position(r.OpPos)
		if p.curLine > 0 && pos.Line > p.curLine {
			p.bslashNewl()
			if !anyNewline {
				p.incLevel()
				anyNewline = true
			}
			p.indent()
		}
		p.didSeparate(pos)
		if p.wantSpace {
			p.space()
		}
		if r.N != nil {
			p.str(r.N.Value)
		}
		p.str(redirectOp(r.Op))
		p.wantSpace = true
		p.word(r.Word)
		if r.Op == SHL || r.Op == DHEREDOC {
			p.pendingHdocs = append(p.pendingHdocs, r)
		}
	}
	if anyNewline {
		p.decLevel()
	}
	if s.Background {
		p.str(" &")
	}
}

func redirectOp(tok Token) string {
	switch tok {
	case LSS:
		return "<"
	case GTR:
		return ">"
	case SHL:
		return "<<"
	case SHR:
		return ">>"
	case RDRINOUT:
		return "<>"
	case DPLIN:
		return "<&"
	case DPLOUT:
		return ">&"
	case DHEREDOC:
		return "<<-"
	case WHEREDOC:
		return "<<<"
	case RDRALL:
		return "&>"
	default: // APPALL
		return "&>>"
	}
}

func binaryCmdOp(tok Token) string {
	switch tok {
	case OR:
		return "|"
	case LAND:
		return "&&"
	case LOR:
		return "||"
	default: // PIPEALL
		return "|&"
	}
}

func caseClauseOp(tok Token) string {
	switch tok {
	case DSEMICOLON:
		return ";;"
	case SEMIFALL:
		return ";&"
	default: // DSEMIFALL
		return ";;&"
	}
}

func (p *printer) command(cmd Command, redirs []*Redirect) (startRedirs int) {
	switch x := cmd.(type) {
	case *CallExpr:
		if len(x.Args) <= 1 {
			p.wordJoin(x.Args, true)
			return 0
		}
		p.wordJoin(x.Args[:1], true)
		for _, r := range redirs {
			if r.Pos() > x.Args[1].Pos() {
				break
			}
			if r.Op == SHL || r.Op == DHEREDOC {
				break
			}
			if p.wantSpace {
				p.space()
			}
			if r.N != nil {
				p.str(r.N.Value)
			}
			p.str(redirectOp(r.Op))
			p.wantSpace = true
			p.word(r.Word)
			startRedirs++
		}
		p.wordJoin(x.Args[1:], true)
	case *Block:
		p.spacedRsrv("{")
		p.nestedStmts(x.Stmts)
		p.semiRsrv("}", x.Rbrace, true)
	case *IfClause:
		p.spacedRsrv("if")
		p.cond(x.Cond)
		p.semiOrNewl("then", x.Then)
		p.nestedStmts(x.ThenStmts)
		for _, el := range x.Elifs {
			p.semiRsrv("elif", el.Elif, true)
			p.cond(el.Cond)
			p.semiOrNewl("then", el.Then)
			p.nestedStmts(el.ThenStmts)
		}
		if len(x.ElseStmts) > 0 {
			p.semiRsrv("else", x.Else, true)
			p.nestedStmts(x.ElseStmts)
		} else if x.Else > 0 {
			p.curLine = p.f.Position(x.Else).Line
		}
		p.semiRsrv("fi", x.Fi, true)
	case *Subshell:
		p.spacedTok("(", false)
		if startsWithLparen(x.Stmts) {
			p.space()
		}
		p.nestedStmts(x.Stmts)
		p.sepTok(")", p.f.Position(x.Rparen))
	case *WhileClause:
		p.spacedRsrv("while")
		p.cond(x.Cond)
		p.semiOrNewl("do", x.Do)
		p.nestedStmts(x.DoStmts)
		p.semiRsrv("done", x.Done, true)
	case *ForClause:
		p.spacedRsrv("for ")
		p.loop(x.Loop)
		p.semiOrNewl("do", x.Do)
		p.nestedStmts(x.DoStmts)
		p.semiRsrv("done", x.Done, true)
	case *BinaryCmd:
		p.stmt(x.X)
		indent := !p.nestedBinary
		if indent {
			p.incLevel()
		}
		_, p.nestedBinary = x.Y.Cmd.(*BinaryCmd)
		ypos := p.f.Position(x.Y.Pos())
		if len(p.pendingHdocs) > 0 {
		} else if ypos.Line > p.curLine {
			p.bslashNewl()
			p.indent()
		}
		p.curLine = ypos.Line
		p.spacedTok(binaryCmdOp(x.Op), true)
		p.stmt(x.Y)
		if indent {
			p.decLevel()
		}
		p.nestedBinary = false
	case *FuncDecl:
		if x.BashStyle {
			p.str("function ")
		}
		p.str(x.Name.Value)
		p.str("() ")
		p.stmt(x.Body)
	case *CaseClause:
		p.spacedRsrv("case ")
		p.word(x.Word)
		p.rsrv(" in")
		p.incLevel()
		for _, pl := range x.List {
			p.didSeparate(p.f.Position(wordFirstPos(pl.Patterns)))
			for i, w := range pl.Patterns {
				if i > 0 {
					p.spacedTok("|", true)
				}
				p.spacedWord(w)
			}
			p.byte(')')
			sep := p.nestedStmts(pl.Stmts)
			p.level++
			opPos := p.f.Position(pl.OpPos)
			if !sep {
				p.curLine++
			} else if opPos.Line == p.curLine && pl.OpPos != x.Esac {
				p.curLine--
			}
			p.sepTok(caseClauseOp(pl.Op), opPos)
			if pl.OpPos == x.Esac {
				p.curLine--
			}
			p.level--
		}
		p.decLevel()
		p.semiRsrv("esac", x.Esac, len(x.List) == 0)
	case *UntilClause:
		p.spacedRsrv("until")
		p.cond(x.Cond)
		p.semiOrNewl("do", x.Do)
		p.nestedStmts(x.DoStmts)
		p.semiRsrv("done", x.Done, true)
	case *DeclClause:
		if x.Local {
			p.spacedRsrv("local")
		} else {
			p.spacedRsrv("declare")
		}
		for _, w := range x.Opts {
			p.spacedWord(w)
		}
		p.assigns(x.Assigns)
	case *EvalClause:
		p.spacedRsrv("eval")
		if x.Stmt != nil {
			p.stmt(x.Stmt)
		}
	case *LetClause:
		p.spacedRsrv("let")
		for _, n := range x.Exprs {
			p.space()
			p.arithm(n, true)
		}
	}
	return startRedirs
}

func startsWithLparen(stmts []*Stmt) bool {
	if len(stmts) < 1 {
		return false
	}
	_, ok := stmts[0].Cmd.(*Subshell)
	return ok
}

func (p *printer) stmts(stmts []*Stmt) bool {
	if len(stmts) == 0 {
		return false
	}
	pos := p.f.Position(stmts[0].Pos())
	if len(stmts) == 1 && pos.Line == p.curLine {
		s := stmts[0]
		p.didSeparate(pos)
		p.stmt(s)
		return false
	}
	inlineIndent := 0
	lastLine := pos.Line
	for i, s := range stmts {
		if i > 0 {
			pos = p.f.Position(s.Pos())
		}
		p.alwaysSeparate(pos)
		p.stmt(s)
		if pos.Line > lastLine+1 {
			inlineIndent = 0
		}
		lastLine = pos.Line
		if !p.hasInline(pos) {
			inlineIndent = 0
			continue
		}
		if inlineIndent == 0 {
			lastLine := pos.Line
			for _, s2 := range stmts[i:] {
				pos2 := p.f.Position(s2.Pos())
				if !p.hasInline(pos2) || pos2.Line > lastLine+1 {
					break
				}
				if l := stmtLen(p.f, s2); l > inlineIndent {
					inlineIndent = l
				}
				lastLine = pos2.Line
			}
		}
		p.wantSpaces = inlineIndent - stmtLen(p.f, s)
	}
	p.wantNewline = true
	return true
}

func unquotedWordStr(f *File, w *Word) string {
	var buf bytes.Buffer
	p := printer{w: &buf, f: f}
	p.unquotedWord(w)
	return buf.String()
}

func wordStr(f *File, w Word) string {
	var buf bytes.Buffer
	p := printer{w: &buf, f: f}
	p.word(w)
	return buf.String()
}

func stmtLen(f *File, s *Stmt) int {
	var buf bytes.Buffer
	p := printer{w: &buf, f: f}
	p.stmt(s)
	return buf.Len()
}

func (p *printer) nestedStmts(stmts []*Stmt) bool {
	p.incLevel()
	sep := p.stmts(stmts)
	p.decLevel()
	return sep
}

func (p *printer) assigns(assigns []*Assign) {
	anyNewline := false
	for _, a := range assigns {
		if p.curLine > 0 && p.f.Position(a.Pos()).Line > p.curLine {
			p.bslashNewl()
			if !anyNewline {
				p.incLevel()
				anyNewline = true
			}
			p.indent()
		} else if p.wantSpace {
			p.space()
		}
		if a.Name != nil {
			p.str(a.Name.Value)
			if a.Append {
				p.token("+=", true)
			} else {
				p.token("=", true)
			}
		}
		p.word(a.Value)
	}
	if anyNewline {
		p.decLevel()
	}
}
