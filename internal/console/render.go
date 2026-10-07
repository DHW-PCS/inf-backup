package console

import (
	"fmt"
	"strings"
)

func formatBytes(value any) string {
	n, ok := number(value)
	if !ok || n < 0 {
		return "未知"
	}
	for _, unit := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if n < 1024 || unit == "TiB" {
			if unit == "B" {
				return fmt.Sprintf("%.0f B", n)
			}
			return fmt.Sprintf("%.1f %s", n, unit)
		}
		n /= 1024
	}
	return "未知"
}

func stringsIn(value any) string {
	items := []string{}
	switch list := value.(type) {
	case []any:
		for _, item := range list {
			if text, ok := item.(string); ok {
				items = append(items, text)
			}
		}
	case []string:
		items = list
	}
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}

func (c *Controller) renderStart(operation string, tags []string) {
	switch operation {
	case "backup":
		c.human("[*] 开始备份: " + c.cfg.TargetPath)
		c.human("[*] 标签: " + strings.Join(tags, ", "))
	case "snapshots":
		c.human("[*] 正在读取仓库快照...")
	case "check":
		c.human("[*] 正在检查仓库一致性...")
	}
}

func (c *Controller) renderProgress(t *task, p map[string]any) {
	percentage := "进行中"
	if n, ok := number(p["percent_done"]); ok {
		percentage = fmt.Sprintf("%d%%", max(0, min(100, int(n*100))))
	}
	files := "文件数统计中"
	if done, ok := number(p["files_done"]); ok {
		if total, ok := number(p["total_files"]); ok {
			files = fmt.Sprintf("%.0f/%.0f 个文件", done, total)
		}
	}
	line := fmt.Sprintf("[~] %s · %s · %s/%s", percentage, files, formatBytes(p["bytes_done"]), formatBytes(p["total_bytes"]))
	if file, ok := p["current_file"].(string); ok && file != "" {
		line += " · 当前: " + file
	}
	if line != t.progressKey {
		t.progressKey = line
		c.human(line)
	}
}

func (c *Controller) renderSuccess(operation string, p map[string]any) {
	switch operation {
	case "backup":
		c.human("[+] 备份完成并创建快照。")
		c.human(fmt.Sprintf("    快照 ID: %v", p["snapshot_id"]))
		count, _ := number(p["total_files_processed"])
		c.human(fmt.Sprintf("    已处理: %.0f 个文件，%s", count, formatBytes(p["total_bytes_processed"])))
		c.human("    新增数据: " + formatBytes(p["data_added"]))
	case "snapshots":
		snapshots, _ := p["snapshots"].([]map[string]any)
		if len(snapshots) == 0 {
			c.human("[+] 仓库中没有快照。")
			return
		}
		c.human(fmt.Sprintf("[+] 共找到 %d 个快照:", len(snapshots)))
		for _, snapshot := range snapshots {
			id, _ := snapshot["short_id"].(string)
			if id == "" {
				id, _ = snapshot["id"].(string)
				id = id[:min(8, len(id))]
			}
			if id == "" {
				id = "unknown"
			}
			stamp, _ := snapshot["time"].(string)
			if stamp == "" {
				stamp = "unknown time"
			}
			host, _ := snapshot["hostname"].(string)
			if host == "" {
				host = "-"
			}
			c.human(fmt.Sprintf("    %s  %s  主机: %s  标签: %s", id, stamp, host, stringsIn(snapshot["tags"])))
			if paths := stringsIn(snapshot["paths"]); paths != "-" {
				c.human("      路径: " + paths)
			}
		}
	case "check":
		c.human("[+] 仓库检查完成，未发现一致性错误。")
		c.human("    错误数: 0")
		c.human("    损坏 pack: 0")
		if p["suggest_prune"] == true {
			c.human("    建议: 可运行 restic prune 回收未使用空间。")
		}
	}
}

func (c *Controller) renderError(operation string, p map[string]any) {
	kind, _ := p["kind"].(string)
	label := map[string]string{
		"repository_locked": "仓库已被锁定", "repository_unavailable": "仓库不存在或无法访问",
		"authentication_failed": "仓库密码或认证失败", "configuration": "wrapper 配置错误",
		"invalid_output": "restic 返回了无法验证的输出", "interrupted": "操作已中止",
	}[kind]
	if label == "" {
		label = "restic " + operation + " 失败"
	}
	c.human(fmt.Sprintf("[!] %s: %v", label, p["message"]))
	guidance := map[string]string{
		"repository_locked":       "请确认没有其他任务后，再人工运行 restic unlock。",
		"repository_unavailable":  "请检查 repository 配置、网络和存储是否可用。",
		"authentication_failed":   "请检查 password_file 路径、权限及文件内容。",
		"configuration":           "请检查 config.yml 以及目标目录和临时目录权限。",
		"invalid_output":          "请检查 Restic 版本和程序日志，不要依赖本次异常输出。",
		"repository_inconsistent": "请保留诊断信息；修复仓库前先检查 Restic 报告。",
	}[kind]
	if guidance != "" {
		c.human("[!] " + guidance)
	}
	if kind == "repository_inconsistent" {
		if details, ok := p["details"].(map[string]any); ok {
			if packs, ok := details["broken_packs"].([]any); ok {
				c.human(fmt.Sprintf("[!] 损坏 pack: %d", len(packs)))
			}
			if details["suggest_repair_index"] == true {
				c.human("[!] Restic 建议评估 repair index；修复前请先保留诊断信息。")
			}
		}
	}
	if p["uncertain"] == true {
		c.human("[!] 结果可能不确定；请检查快照列表后再决定是否重试。")
	}
}
