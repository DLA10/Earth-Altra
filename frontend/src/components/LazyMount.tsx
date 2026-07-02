import { useEffect, useRef, useState, type ReactNode } from "react";

// LazyMount renders its children only once it scrolls near the viewport, and unmounts
// them again when far away. This keeps ~700 candlestick charts from all mounting at
// once — only the ones on screen hold a chart instance.
export function LazyMount({
  children,
  minHeight = 190,
  keepMounted = false,
}: {
  children: ReactNode;
  minHeight?: number;
  keepMounted?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const io = new IntersectionObserver(
      (entries) => {
        const isVisible = entries.some((e) => e.isIntersecting);
        if (isVisible) {
          setVisible(true);
        } else if (!keepMounted) {
          setVisible(false);
        }
      },
      { rootMargin: "300px 0px" }
    );
    io.observe(el);
    return () => io.disconnect();
  }, [keepMounted]);

  return (
    <div ref={ref} style={{ minHeight: visible ? undefined : minHeight }}>
      {visible ? children : null}
    </div>
  );
}
