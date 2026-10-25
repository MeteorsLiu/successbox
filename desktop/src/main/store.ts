import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';

/** Small JSON file for local UI preferences. Never stores credentials. */
export class Store<T extends object> {
  private data: Partial<T>;

  constructor(private readonly file: string) {
    try {
      this.data = JSON.parse(readFileSync(file, 'utf8')) as Partial<T>;
    } catch {
      this.data = {};
    }
  }

  get<K extends keyof T>(key: K): T[K] | undefined {
    return this.data[key];
  }

  set<K extends keyof T>(key: K, value: T[K]) {
    this.data[key] = value;
    mkdirSync(dirname(this.file), { recursive: true });
    writeFileSync(this.file, JSON.stringify(this.data, null, 2));
  }
}
