import React from 'react';
import { NavLink, useNavigate } from 'react-router-dom';
import {
  LayoutDashboard,
  Landmark,
  Bell,
  Activity,
  Server,
  Receipt,
  ExternalLink,
} from 'lucide-react';
import { ViewerRealtimeStatus } from '../viewer/ViewerRealtimeStatus';

export const AdminSidebar: React.FC = () => {
  const navigate = useNavigate();

  const navItems = [
    { to: '/admin/overview', label: 'Tổng quan', icon: LayoutDashboard },
    { to: '/admin/connection', label: 'Kết nối ngân hàng', icon: Landmark },
    { to: '/admin/notifications', label: 'Kênh thông báo', icon: Bell },
    { to: '/admin/activity', label: 'Hoạt động', icon: Activity },
    { to: '/admin/system', label: 'Hệ thống', icon: Server },
  ];

  return (
    <aside className="w-64 bg-white border-r border-stone-200 flex flex-col shrink-0 min-h-screen">
      {/* Brand */}
      <div className="p-6 border-b border-stone-100 flex items-center justify-between">
        <div className="flex items-center gap-2.5">
          <div className="w-8 h-8 rounded-xl bg-stone-900 text-white flex items-center justify-center font-bold text-sm shadow-xs">
            A
          </div>
          <div>
            <span className="text-sm font-bold text-stone-900 leading-tight block">Cổng quản trị</span>
            <span className="text-[11px] font-medium text-stone-500">Monitor &amp; Gateway</span>
          </div>
        </div>
      </div>

      {/* Realtime status badge */}
      <div className="px-5 py-3 border-b border-stone-100 bg-stone-50/50">
        <ViewerRealtimeStatus className="w-full justify-center" />
      </div>

      {/* Main Navigation */}
      <nav className="flex-1 p-4 space-y-1.5 overflow-y-auto">
        {/* Quick jump to Transaction Viewer */}
        <button
          type="button"
          onClick={() => navigate('/transactions')}
          role="button"
          className="w-full flex items-center justify-between px-3.5 py-2.5 rounded-xl text-xs font-semibold text-emerald-800 bg-emerald-50 hover:bg-emerald-100 border border-emerald-200/60 transition cursor-pointer mb-3"
        >
          <div className="flex items-center gap-2.5">
            <Receipt className="w-4 h-4 text-emerald-600" />
            <span>Giao dịch</span>
          </div>
          <ExternalLink className="w-3.5 h-3.5 text-emerald-600" />
        </button>

        <div className="text-[10px] font-bold text-stone-400 uppercase tracking-wider px-3 py-1">
          Menu Quản trị
        </div>

        {navItems.map((item) => {
          const Icon = item.icon;
          return (
            <NavLink
              key={item.to}
              to={item.to}
              role="button"
              className={({ isActive }) =>
                `flex items-center gap-3 px-3.5 py-2.5 rounded-xl text-xs font-medium transition cursor-pointer ${
                  isActive
                    ? 'bg-stone-900 text-white font-semibold shadow-xs'
                    : 'text-stone-600 hover:bg-stone-100 hover:text-stone-900'
                }`
              }
            >
              <Icon className="w-4 h-4" />
              <span>{item.label}</span>
            </NavLink>
          );
        })}

        {/* Quick compatibility buttons for direct E2E test navigation */}
        <div className="pt-4 mt-4 border-t border-stone-100 space-y-1">
          <div className="text-[10px] font-bold text-stone-400 uppercase tracking-wider px-3 py-1">
            Điều hướng nhanh
          </div>
          <button
            type="button"
            role="button"
            onClick={() => navigate('/admin/notifications')}
            className="w-full text-left px-3.5 py-1.5 rounded-lg text-xs text-stone-500 hover:bg-stone-100 transition"
          >
            Webhooks
          </button>
          <button
            type="button"
            role="button"
            onClick={() => navigate('/admin/activity?tab=deliveries')}
            className="w-full text-left px-3.5 py-1.5 rounded-lg text-xs text-stone-500 hover:bg-stone-100 transition"
          >
            Phân phối
          </button>
          <button
            type="button"
            role="button"
            onClick={() => navigate('/admin/system')}
            className="w-full text-left px-3.5 py-1.5 rounded-lg text-xs text-stone-500 hover:bg-stone-100 transition"
          >
            Chẩn đoán
          </button>
          <button
            type="button"
            role="button"
            onClick={() => navigate('/admin/activity?tab=audit')}
            className="w-full text-left px-3.5 py-1.5 rounded-lg text-xs text-stone-500 hover:bg-stone-100 transition"
          >
            Audit
          </button>
        </div>
      </nav>

      {/* Footer */}
      <div className="p-4 border-t border-stone-100 text-[11px] text-stone-400 text-center">
        payOS / KienlongBank
      </div>
    </aside>
  );
};
