import { useEffect, useState } from "react";

/*
 * R4.54：媒体预览加载排队（方案 A/C）。
 *
 * 背景（2026-09-25 实测）：浏览器对同一 HTTP/1.1 源最多 6 条并发连接，
 * 聊天页的图片预览/缩略图/头像都是长连接流，几张慢图就能占满全部 socket，
 * 「加载更早消息」等 API 请求在浏览器内部排队到 80s 超时——后端其实空闲。
 *
 * 方案：预览类 <img> 统一经 useMediaLoadSlot 拿「加载名额」（并发上限 3），
 * 拿到名额才真正设置 src 发起请求；API fetch 不排队（并在 api() 里默认
 * priority: high），从根上保证会话加载优先于预览图。
 * 复用浏览器自身缓存与内存管理（不引入 blob 拷贝），失败/卸载即释放名额。
 */

const MAX_CONCURRENT_MEDIA = 3;

let activeCount = 0;
const waiters = [];

function pump() {
  while (activeCount < MAX_CONCURRENT_MEDIA && waiters.length > 0) {
    const waiter = waiters.shift();
    waiter.fire();
  }
}

// 返回 true 表示已获得加载名额（组件挂载期间持有），false 表示仍在排队。
export function useMediaLoadSlot() {
  const [granted, setGranted] = useState(false);
  useEffect(() => {
    if (granted) {
      activeCount += 1;
      return () => {
        activeCount -= 1;
        pump();
      };
    }
    const waiter = {
      fired: false,
      cancelled: false,
      fire: () => {}
    };
    waiter.fire = () => {
      if (waiter.cancelled) return;
      waiter.fired = true;
      const index = waiters.indexOf(waiter);
      if (index >= 0) waiters.splice(index, 1);
      setGranted(true);
    };
    waiters.push(waiter);
    pump();
    return () => {
      waiter.cancelled = true;
      if (!waiter.fired) {
        const index = waiters.indexOf(waiter);
        if (index >= 0) waiters.splice(index, 1);
      }
    };
  }, [granted]);
  return granted;
}

// 排队加载的预览图：拿到名额前不设 src（不发请求），拿到后才真正加载。
// 其余 props（className/style/onError 等）透传给 <img>，样式行为不变。
export function QueuedImage({ src, alt = "", ...rest }) {
  const granted = useMediaLoadSlot();
  return (
    <img
      src={granted ? src : undefined}
      alt={alt}
      loading="lazy"
      fetchpriority="low"
      {...rest}
    />
  );
}
