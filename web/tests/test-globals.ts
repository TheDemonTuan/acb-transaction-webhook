type GlobalRecord = {
  existed: boolean;
  descriptor?: PropertyDescriptor;
};

const originalDescriptors = new Map<string, GlobalRecord>();

export function setTestGlobal(name: string, value: unknown): void {
  if (!originalDescriptors.has(name)) {
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, name);
    originalDescriptors.set(name, {
      existed: descriptor !== undefined,
      descriptor,
    });
  }

  Object.defineProperty(globalThis, name, {
    value,
    writable: true,
    configurable: true,
    enumerable: true,
  });
}

export function restoreTestGlobals(): void {
  for (const [name, record] of originalDescriptors) {
    if (record.existed && record.descriptor) {
      Object.defineProperty(globalThis, name, record.descriptor);
    } else {
      delete (globalThis as Record<string, unknown>)[name];
    }
  }
  originalDescriptors.clear();
}
