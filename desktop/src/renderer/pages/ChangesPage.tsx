import { useEffect, useState } from 'react';
import { AGENT_NAMES } from '../../shared/agents';
import type { AppState, ChangeRecord, DiffFile } from '../../shared/types';
import { api } from '../api';
import { AgentIcon, ConfirmDialog, Dialog } from '../components/common';
import { useToast } from '../components/Toast';

export function ChangesPage({ state }: { state: AppState }) {
  const toast = useToast();
  const [diffOf, setDiffOf] = useState<ChangeRecord | null>(null);
  const [removing, setRemoving] = useState<ChangeRecord | null>(null);
  const live = state.runs.filter((r) => r.protected);

  const open = (path: string) => toast.run(api.openPath(path));
  const copy = (path: string) =>
    toast.run(navigator.clipboard.writeText(path), '已复制路径');

  return (
    <>
      <div className="note" style={{ marginBottom: 14 }}>
        受保护的 Agent
        结束后，改动会自动保存在这里。原来的目录不会被修改；系统目录的改动会被丢弃。
      </div>

      {live.length > 0 && (
        <>
          <div className="h2">进行中</div>
          <div className="stack">
            {live.map((r) => (
              <div key={r.id} className="card row">
                <AgentIcon agent={r.agent} large />
                <b>{AGENT_NAMES[r.agent]}</b>
                <code>{r.cwd}</code>
                <span className="faint">{r.startedAt} 启动</span>
                <span className="spacer" />
                <span className="muted small">结束后保存改动</span>
              </div>
            ))}
          </div>
        </>
      )}

      <div className="h2">已结束</div>
      {state.changes.length === 0 && (
        <div className="card muted">还没有改动记录</div>
      )}
      <div className="stack">
        {state.changes.map((c) => (
          <div key={c.id} className="card">
            <div className="row">
              <AgentIcon agent={c.agent} large />
              <b>{AGENT_NAMES[c.agent]}</b>
              <span className="muted small">
                {c.provider} · 在 <code>{c.cwd}</code> 中运行
              </span>
              {!c.sessionSynced && (
                <span className="tag todo" title="会话同步回本机正在开发">
                  会话未同步回本机
                </span>
              )}
              <span className="spacer" />
              <span className="faint small">{c.endedAt}</span>
            </div>
            <div style={{ marginLeft: 34, marginTop: 4 }}>
              {c.worktrees.map((w) => (
                <div key={w.path} className="chg-item">
                  <div className="chg-ico wt">⎇</div>
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <div>
                      <b>{w.repo}</b> 的改动已保存为 worktree · {w.files} 个文件{' '}
                      <span style={{ color: 'var(--ok)' }}>+{w.additions}</span>{' '}
                      <span style={{ color: 'var(--err)' }}>
                        −{w.deletions}
                      </span>
                    </div>
                    <div className="mono">{w.path}</div>
                    <div className="row" style={{ marginTop: 6 }}>
                      <button className="btn sm" onClick={() => setDiffOf(c)}>
                        查看改动
                      </button>
                      <button className="btn sm" onClick={() => open(w.path)}>
                        打开文件夹
                      </button>
                      <button className="btn sm" onClick={() => copy(w.path)}>
                        复制路径
                      </button>
                    </div>
                  </div>
                </div>
              ))}
              {c.copies.map((p) => (
                <div key={p.path} className="chg-item">
                  <div className="chg-ico cp">📁</div>
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <div>
                      <b>{p.source}</b> 不是 Git 仓库，改动已保存为副本 ·{' '}
                      {p.files} 个文件
                    </div>
                    <div className="mono">{p.path}</div>
                    <div className="row" style={{ marginTop: 6 }}>
                      <button className="btn sm" onClick={() => open(p.path)}>
                        打开文件夹
                      </button>
                      <button className="btn sm" onClick={() => copy(p.path)}>
                        复制路径
                      </button>
                    </div>
                  </div>
                </div>
              ))}
              {c.discarded > 0 && (
                <div className="chg-item">
                  <div className="chg-ico dc">⌫</div>
                  <div className="muted" style={{ flex: 1 }}>
                    系统目录中的 {c.discarded} 处改动已丢弃（例如安装到{' '}
                    <code>/usr/local</code> 的软件）
                  </div>
                </div>
              )}
              {!c.worktrees.length && !c.copies.length && !c.discarded && (
                <div className="chg-item muted">没有改动</div>
              )}
            </div>
            <div
              className="row"
              style={{ marginTop: 8, justifyContent: 'flex-end' }}
            >
              <button
                className="btn sm ghost danger"
                onClick={() => setRemoving(c)}
              >
                删除这条记录和保存的文件
              </button>
            </div>
          </div>
        ))}
      </div>

      {diffOf && <DiffDialog change={diffOf} onClose={() => setDiffOf(null)} />}
      {removing && (
        <ConfirmDialog
          title="删除这条记录？"
          confirm="删除"
          onClose={() => setRemoving(null)}
          onConfirm={() => toast.run(api.deleteChange(removing.id), '已删除')}
        >
          <p>会删除保存的 worktree 和副本文件夹。原来的目录不受影响。</p>
        </ConfirmDialog>
      )}
    </>
  );
}

function DiffDialog({
  change,
  onClose,
}: {
  change: ChangeRecord;
  onClose(): void;
}) {
  const toast = useToast();
  const [files, setFiles] = useState<DiffFile[] | null>(null);
  const [selected, setSelected] = useState(0);

  useEffect(() => {
    api.getDiff(change.id).then(setFiles, (err: Error) => {
      toast.show(err.message);
      onClose();
    });
  }, [change.id]);

  const file = files?.[selected];
  return (
    <Dialog
      size="xwide"
      onClose={onClose}
      title=""
      head={
        <div className="m-head row" style={{ paddingBottom: 12 }}>
          <h3>改动</h3>
          <span className="muted">
            {change.worktrees[0]?.repo} · 与原仓库 HEAD 比较
          </span>
          <span className="spacer" />
          <button className="btn sm" onClick={onClose}>
            关闭
          </button>
        </div>
      }
    >
      <div className="diff">
        <div className="diff-files">
          {files?.map((f, i) => (
            <button
              key={f.path}
              className={i === selected ? 'on' : ''}
              onClick={() => setSelected(i)}
            >
              <span className={`st ${f.status}`}>{f.status}</span>
              {f.path.split('/').pop()}
              <span className="spacer" />
              <span style={{ color: 'var(--ok)' }}>+{f.additions}</span>
              <span style={{ color: 'var(--err)' }}>−{f.deletions}</span>
            </button>
          ))}
        </div>
        <div className="diff-body">
          {!files && (
            <div className="ln muted">
              <i className="spin" /> 正在读取…
            </div>
          )}
          {file && (
            <>
              <div className="ln h">{file.path}</div>
              {file.lines.map((l, i) => (
                <div
                  key={i}
                  className={`ln ${l.kind === '@' ? 'h' : l.kind === '+' ? 'a' : l.kind === '-' ? 'r' : ''}`}
                >
                  {l.kind === '@' ? '' : l.kind}
                  {l.text}
                </div>
              ))}
            </>
          )}
        </div>
      </div>
    </Dialog>
  );
}
