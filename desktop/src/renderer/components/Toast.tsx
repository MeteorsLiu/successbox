import {
  createContext,
  useCallback,
  useContext,
  useState,
  type ReactNode,
} from 'react';

interface Toast {
  id: number;
  message: string;
  error: boolean;
}

interface ToastApi {
  show(message: string): void;
  /** Run an api call; show `success` when it resolves and the error message when it fails. */
  run(task: Promise<unknown>, success?: string): Promise<boolean>;
}

const ToastContext = createContext<ToastApi | null>(null);
let nextId = 0;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const push = useCallback((message: string, error = false) => {
    const id = nextId++;
    setToasts((list) => [...list, { id, message, error }]);
    setTimeout(
      () => setToasts((list) => list.filter((t) => t.id !== id)),
      error ? 4000 : 2600,
    );
  }, []);

  const run = useCallback(
    async (task: Promise<unknown>, success?: string) => {
      try {
        await task;
        if (success) push(success);
        return true;
      } catch (err) {
        push(err instanceof Error ? err.message : String(err), true);
        return false;
      }
    },
    [push],
  );

  return (
    <ToastContext.Provider value={{ show: push, run }}>
      {children}
      <div className="toasts">
        {toasts.map((t) => (
          <div key={t.id} className={`toast ${t.error ? 'error' : ''}`}>
            {t.message}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast() {
  const toast = useContext(ToastContext);
  if (!toast) throw new Error('useToast must be used inside <ToastProvider>');
  return toast;
}
