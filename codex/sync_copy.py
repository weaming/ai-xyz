#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///

"""校对并同步 copy 目录中的上游 Codex 文件。"""

from __future__ import annotations

import argparse
import hashlib
import json
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

DEFAULT_OFFICIAL_ROOT = Path.home() / 'src/codex/codex-rs'
DEFAULT_BUILD_MANIFESTS = (
    Path('codex-responses-api/Cargo.toml'),
    Path('mcp/Cargo.toml'),
)


@dataclass(frozen=True)
class FileRef:
    copy_path: Path
    official_path: Path


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description='校对并同步 copy 目录中的上游 Codex 文件')
    parser.add_argument(
        '--official-root',
        type=Path,
        default=DEFAULT_OFFICIAL_ROOT,
        help='官方 codex 仓库根目录',
    )
    parser.add_argument(
        '--refs',
        type=Path,
        default=Path('copy/refs.json'),
        help='refs.json 路径',
    )
    parser.add_argument(
        '--check',
        action='store_true',
        help='只校验 hash，不同步文件',
    )
    parser.add_argument(
        '--build-manifest',
        dest='build_manifests',
        action='append',
        type=Path,
        help='同步后执行 cargo check 的 manifest 路径，可重复指定',
    )
    parser.add_argument(
        '--no-build',
        action='store_true',
        help='跳过同步后的 cargo check',
    )
    return parser.parse_args()


def load_refs(refs_path: Path) -> list[FileRef]:
    try:
        data = json.loads(refs_path.read_text(encoding='utf-8'))
    except OSError as error:
        raise RuntimeError(f'读取 refs.json 失败: {refs_path}: {error}') from error
    except json.JSONDecodeError as error:
        raise RuntimeError(f'refs.json 格式无效: {refs_path}: {error}') from error

    if not isinstance(data, list) or not data:
        raise RuntimeError('refs.json 必须是非空数组')

    refs: list[FileRef] = []
    for index, item in enumerate(data):
        if not isinstance(item, dict):
            raise TypeError(f'refs.json 第 {index} 项必须是对象')
        copy_path = item.get('copy')
        official_path = item.get('official')
        if not isinstance(copy_path, str) or not isinstance(official_path, str):
            raise TypeError(f'refs.json 第 {index} 项必须包含字符串 copy/official')
        refs.append(FileRef(Path(copy_path), Path(official_path)))
    return refs


def resolve_root(path: Path, base_dir: Path) -> Path:
    expanded_path = path.expanduser()
    return expanded_path.resolve() if expanded_path.is_absolute() else (base_dir / expanded_path).resolve()


def safe_join(root: Path, relative_path: Path, label: str) -> Path:
    if relative_path.is_absolute():
        raise RuntimeError(f'{label} 路径必须是相对路径: {relative_path}')
    path = (root / relative_path).resolve()
    try:
        path.relative_to(root)
    except ValueError as error:
        raise RuntimeError(f'{label} 路径越过根目录: {relative_path}') from error
    return path


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open('rb') as file:
            for chunk in iter(lambda: file.read(1024 * 1024), b''):
                digest.update(chunk)
    except OSError as error:
        raise RuntimeError(f'读取文件失败: {path}: {error}') from error
    return digest.hexdigest()


def compare_refs(refs: list[FileRef], copy_root: Path, official_root: Path) -> list[tuple[Path, Path]]:
    changed: list[tuple[Path, Path]] = []
    for ref in refs:
        copy_path = safe_join(copy_root, ref.copy_path, 'copy')
        official_path = safe_join(official_root, ref.official_path, 'official')
        if not official_path.is_file():
            raise RuntimeError(f'官方文件不存在: {official_path}')
        if not copy_path.is_file():
            changed.append((official_path, copy_path))
            print(f'missing: {ref.copy_path}')
            continue

        official_hash = sha256(official_path)
        copy_hash = sha256(copy_path)
        if official_hash == copy_hash:
            print(f'ok: {ref.copy_path} {copy_hash[:12]}')
            continue

        changed.append((official_path, copy_path))
        print(f'changed: {ref.copy_path} copy={copy_hash[:12]} official={official_hash[:12]}')
    return changed


def sync_files(changed: list[tuple[Path, Path]]) -> None:
    for official_path, copy_path in changed:
        copy_path.parent.mkdir(parents=True, exist_ok=True)
        command = ['cp', str(official_path), str(copy_path)]
        print(f'sync: {official_path} -> {copy_path}')
        try:
            subprocess.run(command, check=True)
        except OSError as error:
            raise RuntimeError(f'执行 cp 失败: {error}') from error
        except subprocess.CalledProcessError as error:
            raise RuntimeError(f'cp 返回失败状态: {error.returncode}') from error


def verify_synced_files(refs: list[FileRef], copy_root: Path, official_root: Path) -> None:
    changed = compare_refs(refs, copy_root, official_root)
    if changed:
        raise RuntimeError('同步后仍存在 hash 差异')


def run_build(manifest: Path, repo_root: Path) -> None:
    manifest_path = resolve_root(manifest, repo_root)
    if not manifest_path.is_file():
        raise RuntimeError(f'cargo manifest 不存在: {manifest_path}')
    print(f'build: cargo check --manifest-path {manifest_path}')
    try:
        subprocess.run(
            ['cargo', 'check', '--manifest-path', str(manifest_path)],
            cwd=repo_root,
            check=True,
        )
    except OSError as error:
        raise RuntimeError(f'执行 cargo check 失败: {error}') from error
    except subprocess.CalledProcessError as error:
        raise RuntimeError(f'cargo check 返回失败状态: {error.returncode}') from error


def main() -> int:
    args = parse_args()
    repo_root = Path(__file__).resolve().parent
    refs_path = resolve_root(args.refs, repo_root)
    copy_root = refs_path.parent
    official_root = resolve_root(args.official_root, repo_root)
    refs = load_refs(refs_path)
    changed = compare_refs(refs, copy_root, official_root)

    if not changed:
        print('all copied files are up to date')
        return 0
    if args.check:
        print(f'hash check failed: {len(changed)} file(s) differ', file=sys.stderr)
        return 1

    sync_files(changed)
    verify_synced_files(refs, copy_root, official_root)
    if not args.no_build:
        build_manifests = args.build_manifests or DEFAULT_BUILD_MANIFESTS
        for manifest in build_manifests:
            run_build(manifest, repo_root)
    print(f'synced {len(changed)} file(s)')
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (RuntimeError, TypeError) as error:
        print(f'error: {error}', file=sys.stderr)
        raise SystemExit(1) from error
