import { useEffect, type ReactNode } from 'react';
import type { AgentId } from '../../shared/types';

export function Switch(props: {
  checked: boolean;
  onChange(checked: boolean): void;
  small?: boolean;
  blue?: boolean;
  title?: string;
}) {
  return (
    <label
      className={`sw ${props.small ? 'sm' : ''} ${props.blue ? 'blue' : ''}`}
      title={props.title}
    >
      <input
        type="checkbox"
        checked={props.checked}
        onChange={(e) => props.onChange(e.target.checked)}
        aria-label={props.title}
      />
      <i />
    </label>
  );
}

export function AgentIcon({
  agent,
  large,
}: {
  agent: AgentId;
  large?: boolean;
}) {
  return (
    <span className={`ag ${agent} ${large ? 'lg' : ''}`}>
      {agent === 'claude' ? '✻' : '◎'}
    </span>
  );
}

export function Dialog(props: {
  title: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  size?: 'wide' | 'xwide';
  onClose(): void;
  head?: ReactNode;
}) {
  const { onClose } = props;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);
  return (
    <div
      className="scrim"
      onMouseDown={(e) => e.target === e.currentTarget && onClose()}
    >
      <div className={`modal ${props.size ?? ''}`} role="dialog">
        {props.head ?? (
          <div className="m-head">
            <h3>{props.title}</h3>
          </div>
        )}
        {props.size === 'xwide' ? (
          props.children
        ) : (
          <div className="m-body">{props.children}</div>
        )}
        {props.footer && <div className="m-foot">{props.footer}</div>}
      </div>
    </div>
  );
}

export function ConfirmDialog(props: {
  title: string;
  children: ReactNode;
  confirm: string;
  onConfirm(): void;
  onClose(): void;
}) {
  return (
    <Dialog
      title={props.title}
      onClose={props.onClose}
      footer={
        <>
          <span className="spacer" />
          <button className="btn" onClick={props.onClose}>
            取消
          </button>
          <button
            className="btn primary"
            onClick={() => {
              props.onConfirm();
              props.onClose();
            }}
          >
            {props.confirm}
          </button>
        </>
      }
    >
      {props.children}
    </Dialog>
  );
}
