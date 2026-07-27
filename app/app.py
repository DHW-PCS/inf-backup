import sys
import os
import json
from datetime import datetime
import yaml
import multiprocessing
import restic
import restic.errors

__version__ = "1.0.0"

def _get_restic_version():
    """返回已安装的 restic 二进制版本字符串"""
    import subprocess
    try:
        result = subprocess.run(["restic", "version"], capture_output=True, text=True)
        return result.stdout.strip().split(chr(10))[0]
    except Exception:
        return "restic (unknown)"


def load_config(config_path="/home/container/config.yml"):
    """加载并解析 YAML 配置文件"""
    try:
        with open(config_path, 'r', encoding='utf-8') as f:
            return yaml.safe_load(f)
    except FileNotFoundError:
        print(f"[!] 错误: 找不到配置文件 {config_path}")
        sys.exit(1)
    except Exception as e:
        print(f"[!] 解析配置文件时出错: {e}")
        sys.exit(1)

def _handle_restic_error(command, error_str):
    """解析 restic 的 JSON 错误输出并给出可操作的提示"""
    # 尝试从错误字符串中提取 JSON 消息体
    json_start = error_str.find('{')
    if json_start != -1:
        try:
            err_data = json.loads(error_str[json_start:])
            code = err_data.get('code')
            msg = err_data.get('message', '')

            if code == 11:
                print(f"[!] 仓库已被锁定: {msg}")
                print("[!] 提示: 请先运行 restic unlock 清除残留锁")
                return
            elif code == 10:
                print(f"[!] 仓库不存在或无法访问: {msg}")
                return
            elif code == 12:
                print(f"[!] 密码错误: {msg}")
                print("[!] 提示: 请检查密码文件内容是否正确")
                return
        except (json.JSONDecodeError, KeyError):
            pass

    # 无法解析具体错误码时，检查常见错误模式
    if 'already locked' in error_str or 'lock' in error_str.lower():
        print(f"[!] 仓库操作被锁定: {error_str.split(chr(10))[0]}")
        print("[!] 提示: 请先运行 restic unlock 清除残留锁")
    elif 'unable to open repository' in error_str:
        print(f"[!] 无法打开仓库，请检查仓库地址与后端连接")
    elif 'password' in error_str.lower():
        print(f"[!] 密码或认证失败，请检查密码文件")
    else:
        # 只显示第一条错误信息，避免冗长的堆栈
        first_line = error_str.split(chr(10))[0]
        print(f"[!] restic {command} 失败: {first_line}")


def task_worker(command, args, repo_config):
    """子进程：执行具体的操作（restic 命令或本地辅助命令）"""
    try:
        target = repo_config['target_path']
        
        # 从配置中设置 TMPDIR 环境变量
        if 'tmp_dir' in repo_config:
            os.environ['TMPDIR'] = repo_config['tmp_dir']

        # 本地命令：ls（列出目标目录第一层内容）
        if command == "ls":
            path = args[0] if args else target
            if not os.path.exists(path):
                print(f"[!] 错误: 目录不存在: {path}")
                return
            if not os.path.isdir(path):
                print(f"[!] 错误: 不是目录: {path}")
                return

            print(f"[*] 列出目录第一层: {path}")
            try:
                entries = []
                for entry in os.scandir(path):
                    name = entry.name
                    # 默认不显示隐藏项（类似 ls）
                    if name.startswith('.'):
                        continue
                    if entry.is_dir(follow_symlinks=False):
                        name += '/'
                    elif entry.is_symlink():
                        name += '@'
                    elif entry.is_file() and os.access(entry.path, os.X_OK):
                        name += '*'
                    entries.append(name)
                entries.sort()
                if entries:
                    print("\n".join(entries))
                else:
                    print("[?] 目录为空。")
            except PermissionError:
                print("[!] 权限不足，无法访问该目录内容。")
            return

        # 需要 restic 的命令：仅在需要时配置 restic 环境
        if command in ("backup", "check", "snapshots"):
            # 设置 resticpy 全局配置
            restic.repository = repo_config['repository']
            restic.password_file = repo_config.get('password_file')
            # 如果配置中没有 password_file 但有 password，需要警告
            if not restic.password_file and 'password' in repo_config:
                print("[!] 警告: resticpy 需要 password_file 而不是 password 字符串")
                print("[!] 请在配置文件中设置 password_file 路径")
                return

        if command == "backup":
            timestamp_tag = datetime.now().strftime("%Y%m%d%H%M%S")
            tags = list(args) + [timestamp_tag]
            print(f"[*] 开始备份 {target}，标签: {tags}...")
            result = restic.backup(paths=[target], tags=tags, exclude_patterns=repo_config.get('exclude_patterns', []))
            print(f"[+] 备份成功:\n{result}")

        elif command == "check":
            print("[*] 正在执行仓库一致性检查...")
            result = restic.check()
            print(f"[+] 检查完成:\n{result}")

        elif command == "snapshots":
            print("[*] 正在获取快照列表...")
            result = restic.snapshots()
            print(f"[+] 快照列表:\n{result}")

        # elif command == "restore":
        #     snapshot_id = args[0] if args else 'latest'
        #     print(f"[*] 正在将快照 {snapshot_id} 恢复至 {target}...")
        #     # resticpy 的 restore 使用 target_dir 参数
        #     result = restic.restore(snapshot_id=snapshot_id, target_dir=target, exclude=repo_config.get('exclude_patterns', []))
        #     print(f"[+] 恢复完成:\n{result}")

    except restic.errors.ResticFailedError as e:
        _handle_restic_error(command, str(e))
    except restic.errors.Error as e:
        print(f"[!] restic 内部错误: {e}")
    except Exception as e:
        print(f"[!] 执行 {command} 时发生未预期错误: {e}")

def main():
    config = load_config()
    current_process = None

    print(f"=== Restic 备份控制台 v{__version__} ===")
    print(f"当前仓库: {config['repository']}")
    print(f"目标目录: {config['target_path']}")
    print("支持命令: backup <tags>, check, snapshots, ls, version, halt, stop")
    # Pterodactyl 通过匹配 "Done" 来识别服务器已启动完成并标记为 running 状态，请勿删除此行
    print("[Server thread/INFO]: Done (114.514s)! For help, type \"help\"")
    print("----------------------------")

    while True:
        try:
            # 读取控制台输入
            line = sys.stdin.readline()
            if not line:
                break
            
            parts = line.strip().split()
            if not parts:
                continue

            cmd = parts[0].lower()
            args = parts[1:]

            # 1. 处理中止指令 (Halt)
            if cmd == "halt":
                if current_process and current_process.is_alive():
                    print("[!] 收到 halt 命令，正在中止当前操作...")
                    current_process.terminate()
                    current_process.join()
                    print("[-] 操作已强制停止。")
                else:
                    print("[?] 当前没有正在运行的任务。")
                continue

            # 2. 处理退出程序
            if cmd == "stop":
                if current_process and current_process.is_alive():
                    current_process.terminate()
                print("程序已退出。")
                break

            # 3. 检查任务冲突
            if current_process and current_process.is_alive():
                print("[!] 警告: 上一个任务仍在运行。请等待完成或输入 'halt' 中止。")
                continue

            # 4. 路由命令到子进程
            if cmd == "version":
                print(f"Wrapper:  v{__version__}")
                print(f"Restic:   {_get_restic_version()}")
                continue

            if cmd in ["backup", "check", "snapshots", "ls"]:
                # if cmd == "restore" and not args:
                #     print("[!] 错误: restore 命令需要快照 ID。")
                #     continue

                # 创建并启动进程
                current_process = multiprocessing.Process(
                    target=task_worker, 
                    args=(cmd, args, config)
                )
                current_process.start()
            else:
                print(f"[?] 未知命令: {cmd}")

        except KeyboardInterrupt:
            if current_process and current_process.is_alive():
                current_process.terminate()
            break

if __name__ == "__main__":
    main()